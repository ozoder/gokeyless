package client

import (
	"container/heap"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/cloudflare/cfssl/log"
	"github.com/cloudflare/gokeyless"
	"github.com/miekg/dns"
)

// A Remote represents some number of remote keyless server(s)
type Remote interface {
	Dial(*Client) (*gokeyless.Conn, error)
	Add(Remote) Remote
}

// A singleRemote is an individual remote server
type singleRemote struct {
	net.Addr
	ServerName string
	conn       *gokeyless.Conn
}

// NewServer creates a new remote based a given addr and server name.
func NewServer(addr net.Addr, serverName string) Remote {
	return &singleRemote{
		Addr:       addr,
		ServerName: serverName,
	}
}

func (c *Client) lookupIPs(host string) (ips []net.IP, err error) {
	m := new(dns.Msg)
	for _, resolver := range c.Resolvers {
		m.SetQuestion(dns.Fqdn(host), dns.TypeA)
		if in, err := dns.Exchange(m, resolver); err == nil {
			for _, rr := range in.Answer {
				if a, ok := rr.(*dns.A); ok {
					ips = append(ips, a.A)
				}
			}
		} else {
			log.Debug(err)
		}

		m.SetQuestion(dns.Fqdn(host), dns.TypeAAAA)
		if in, err := dns.Exchange(m, resolver); err == nil {
			for _, rr := range in.Answer {
				if aaaa, ok := rr.(*dns.AAAA); ok {
					ips = append(ips, aaaa.AAAA)
				}
			}
		} else {
			log.Debug(err)
		}
	}
	if len(ips) != 0 {
		return ips, nil
	}

	return net.LookupIP(host)
}

// LookupServerWithName uses DNS to look up an a group of Remote servers with
// optional TLS server name.
func (c *Client) LookupServerWithName(serverName, host string, port int) (Remote, error) {
	if serverName == "" {
		serverName = host
	}

	ips, err := c.lookupIPs(host)
	if err != nil {
		return nil, err
	}

	var servers []Remote
	for _, ip := range ips {
		addr := &net.TCPAddr{IP: ip, Port: port}
		if !c.Blacklist.Contains(addr) {
			servers = append(servers, NewServer(addr, serverName))
		}
	}
	return NewGroup(servers)
}

// LookupServer with default ServerName.
func (c *Client) LookupServer(hostport string) (Remote, error) {
	host, p, err := net.SplitHostPort(hostport)
	if err != nil {
		return nil, err
	}

	port, err := strconv.Atoi(p)
	if err != nil {
		return nil, err
	}

	return c.LookupServerWithName(host, host, port)
}

// Dial dials a remote server, returning an existing connection if possible.
func (s *singleRemote) Dial(c *Client) (*gokeyless.Conn, error) {
	if c.Blacklist.Contains(s) {
		return nil, fmt.Errorf("server %s on client blacklist", s.String())
	}

	if s.conn != nil && s.conn.Use() {
		return s.conn, nil
	}

	config := copyTLSConfig(c.Config)
	config.ServerName = s.ServerName
	log.Debugf("Dialing %s at %s\n", s.ServerName, s.String())
	inner, err := tls.DialWithDialer(c.Dialer, s.Network(), s.String(), config)
	if err != nil {
		return nil, err
	}

	s.conn = gokeyless.NewConn(inner)
	return s.conn, nil
}

func (s *singleRemote) Add(r Remote) Remote {
	g, _ := NewGroup([]Remote{s, r})
	return g
}

func copyTLSConfig(c *tls.Config) *tls.Config {
	return &tls.Config{
		Certificates:             c.Certificates,
		NameToCertificate:        c.NameToCertificate,
		GetCertificate:           c.GetCertificate,
		RootCAs:                  c.RootCAs,
		NextProtos:               c.NextProtos,
		ServerName:               c.ServerName,
		ClientAuth:               c.ClientAuth,
		ClientCAs:                c.ClientCAs,
		InsecureSkipVerify:       c.InsecureSkipVerify,
		CipherSuites:             c.CipherSuites,
		PreferServerCipherSuites: c.PreferServerCipherSuites,
		SessionTicketsDisabled:   c.SessionTicketsDisabled,
		SessionTicketKey:         c.SessionTicketKey,
		ClientSessionCache:       c.ClientSessionCache,
		MinVersion:               c.MinVersion,
		MaxVersion:               c.MaxVersion,
		CurvePreferences:         c.CurvePreferences,
	}
}

// ewmaLatency is exponentially weighted moving average of latency
type ewmaLatency struct {
	val      time.Duration
	measured bool
}

func (l ewmaLatency) Update(val time.Duration) {
	l.val /= 2
	l.val += (val / 2)
}

func (l ewmaLatency) Reset() {
	l.val = 0
	l.measured = false
}

func (l ewmaLatency) Better(r ewmaLatency) bool {
	// if l is not measured (it also means last measurement was
	// a failure), any updated/measured latency is better than
	// l. Also if neither l or r is measured, l can't be better
	// than r.
	if !l.measured {
		return false
	}

	if l.measured && !r.measured {
		return true
	}

	return l.val < r.val
}

type item struct {
	Remote
	index      int
	latency    ewmaLatency
	errorCount int
}

// A Group is a Remote consisting of a load-balanced set of external servers.
type Group struct {
	sync.Mutex
	remotes []*item
}

// NewGroup creates a new group from a set of remotes.
func NewGroup(remotes []Remote) (*Group, error) {
	if len(remotes) == 0 {
		return nil, errors.New("attempted to create empty remote group")
	}
	g := new(Group)
	for _, r := range remotes {
		heap.Push(g, &item{Remote: r})
	}

	return g, nil
}

// Dial returns a connection with best latency measurement.
func (g *Group) Dial(c *Client) (conn *gokeyless.Conn, err error) {
	g.Lock()
	defer g.Unlock()

	if g.Len() == 0 {
		err = errors.New("remote group empty")
		return
	}

	var i *item
	var popped []*item
	for g.Len() > 0 {
		i = heap.Pop(g).(*item)
		popped = append(popped, i)
		conn, err = i.Dial(c)
		if err == nil {
			break
		}

		log.Debug(err)
		i.latency.Reset()
		i.errorCount++
	}

	for _, f := range popped {
		heap.Push(g, f)
	}

	// fail to find a usable connection
	if err != nil {
		return nil, err
	}

	// loop through all remote servers for performance measurement
	// in a separate goroutine
	go func() {
		time.Sleep(100 * time.Microsecond)
		g.Lock()
		for _, i := range g.remotes {
			conn, err := i.Dial(c)
			if err != nil {
				i.latency.Reset()
				i.errorCount++
				log.Infof("Dial failed: %v", err)
				continue
			}

			start := time.Now()
			err = conn.Ping(nil)
			duration := time.Since(start)

			if err != nil {
				i.latency.Reset()
				i.errorCount++
				log.Infof("Ping failed: %v", err)
			} else {
				log.Debug("ping duration:", duration)
				i.latency.Update(duration)
			}
			defer conn.Close()
		}
		sort.Sort(g)

		g.Unlock()
	}()

	return conn, nil
}

// Add adds r into the underlying Remote list
func (g *Group) Add(r Remote) Remote {
	if g != r {
		heap.Push(g, &item{Remote: r})
	}
	return g
}

// Len(), Less(i, j) and Swap(i,j) implements sort.Interface

// Len returns the number of remote
func (g *Group) Len() int {
	return len(g.remotes)
}

// Swap swaps remote i and remote j in the list
func (g *Group) Swap(i, j int) {
	g.remotes[i], g.remotes[j] = g.remotes[j], g.remotes[i]
	g.remotes[i].index = i
	g.remotes[j].index = j
}

// Less compares two Remotes at position i and j based on latency
func (g *Group) Less(i, j int) bool {
	// TODO: incorporate more logic about open connections and failure rate
	pi, pj := g.remotes[i].latency, g.remotes[j].latency
	errsi, errsj := g.remotes[i].errorCount, g.remotes[j].errorCount

	return pi.Better(pj) || pi == pj && errsi < errsj
}

// With above implemented sort.Interface, Push and Pop completes
// heap.Interface, Now type item can be heap sorted and the
// top of the heap would have minimal measured latency.

// Push pushes x into the remote lists
func (g *Group) Push(x interface{}) {
	i := x.(*item)
	i.index = len(g.remotes)
	g.remotes = append(g.remotes, i)
}

// Pop drops the last Remote object from the list
// Note: this is different from heap.Pop().
func (g *Group) Pop() interface{} {
	i := g.remotes[len(g.remotes)-1]
	g.remotes = g.remotes[0 : len(g.remotes)-1]
	return i
}