// Package meshdns answers .voodu names across voodu hosts.
//
// Every host runs one inside its controller. A container asks it for any
// name docker's embedded DNS could not answer: a .voodu name that lives on
// another host is asked of the other hosts — the owner answers — and every
// other name goes to the host's own resolver. Another host asks it only
// about this host's containers.
//
// Nothing is copied between hosts. An answer always comes from the host
// that owns the container, so it is never stale, and the transport is DNS
// itself.
package meshdns

import (
	"context"
	"encoding/binary"
	"io"
	"math/rand/v2"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

const (
	// answerTTL is short on purpose: a deployment's addresses change on
	// every deploy.
	answerTTL = 5

	// negativeTTL keeps a name nobody has — a typo, a stopped service —
	// from sending a round to every peer on every lookup.
	negativeTTL = 2 * time.Second

	// maxUDPResponse is the classic DNS limit. A bigger answer goes out
	// truncated and the client asks again over TCP.
	maxUDPResponse = 512
)

// Index answers this host's own names. name is lowercase and fully
// qualified ("pg-0.contagorda.voodu.").
type Index interface {
	Lookup(name string) []netip.Addr
}

// Peers lists where the other hosts' mesh DNS listens.
type Peers interface {
	List() []netip.AddrPort
}

// Server answers for one host. The zero value is not usable: Local,
// Tunnel, Index and Peers are required.
type Server struct {
	// Local is voodu0's subnet: a query from it comes from a container on
	// this host.
	Local netip.Prefix

	// Tunnel is the voodu WireGuard network: a query from it comes from
	// another host.
	Tunnel netip.Prefix

	Index Index
	Peers Peers

	// Upstreams are the host's own resolvers, asked in order for every
	// name outside the mesh.
	Upstreams []netip.AddrPort

	// PeerTimeout bounds one round to the peers; UpstreamTimeout one
	// question to an upstream. Zero means 300ms and 2s.
	PeerTimeout     time.Duration
	UpstreamTimeout time.Duration

	Logf func(format string, args ...any)

	// now is swapped by tests.
	now func() time.Time

	mu    sync.Mutex
	cache map[string]cached
}

type cached struct {
	addrs []netip.Addr
	until time.Time
}

// Handle answers one DNS message that arrived from src over UDP or TCP.
// nil means drop it.
func (s *Server) Handle(ctx context.Context, src netip.Addr, req []byte, tcp bool) []byte {
	var p dnsmessage.Parser

	hdr, err := p.Start(req)
	if err != nil || hdr.Response {
		return nil
	}

	q, err := p.Question()
	if err != nil {
		return reply(hdr, nil, dnsmessage.RCodeFormatError, nil, tcp)
	}

	src = src.Unmap()
	local := s.Local.Contains(src)
	peer := s.Tunnel.Contains(src)

	// Anything else reached us by accident: Linux accepts a packet for a
	// local address on any interface, so the tunnel address is reachable
	// from the provider's network. It must not become an open resolver.
	if !local && !peer {
		return reply(hdr, &q, dnsmessage.RCodeRefused, nil, tcp)
	}

	name := strings.ToLower(q.Name.String())

	if !strings.HasSuffix(name, ".voodu.") {
		// Only this host's containers get other names resolved — a peer
		// passing internet names through us would be the open resolver
		// again.
		if peer {
			return reply(hdr, &q, dnsmessage.RCodeRefused, nil, tcp)
		}

		return s.forward(ctx, hdr, &q, req, tcp)
	}

	addrs := s.Index.Lookup(name)

	// Only a container asks the other hosts, and only for an A: the AAAA a
	// resolver sends alongside can carry no answer here, so it must not
	// cost a round of the mesh. A peer asking us gets our own names and
	// nothing more, so a question never travels in a loop.
	if len(addrs) == 0 && local && q.Type == dnsmessage.TypeA {
		addrs = s.remote(ctx, name)
	}

	if len(addrs) == 0 {
		return reply(hdr, &q, dnsmessage.RCodeNameError, nil, tcp)
	}

	// The name exists. Only A carries an answer; the AAAA a resolver asks
	// for alongside gets "no data" — "no such name" would read, to some
	// resolvers, as a denial of the A as well.
	if q.Type != dnsmessage.TypeA {
		return reply(hdr, &q, dnsmessage.RCodeSuccess, nil, tcp)
	}

	return reply(hdr, &q, dnsmessage.RCodeSuccess, shuffled(addrs), tcp)
}

// Resolve answers name the way a local container's query would be
// answered: this host's index first, then the other hosts. For the
// controller's own lookups — an ingress whose service lives on another
// host. name is a fully-qualified .voodu name, with or without the
// trailing dot.
func (s *Server) Resolve(ctx context.Context, name string) []netip.Addr {
	name = strings.ToLower(name)

	if !strings.HasSuffix(name, ".") {
		name += "."
	}

	if !strings.HasSuffix(name, ".voodu.") {
		return nil
	}

	if addrs := s.Index.Lookup(name); len(addrs) > 0 {
		return addrs
	}

	return s.remote(ctx, name)
}

// remote asks every peer for name and returns what they answered, merged.
// More than one host answering is legitimate — the same service on two
// hosts — but worth saying, since it is also what two environments sharing
// a scope look like.
func (s *Server) remote(ctx context.Context, name string) []netip.Addr {
	if addrs, ok := s.fromCache(name); ok {
		return addrs
	}

	peers := s.Peers.List()

	ctx, cancel := context.WithTimeout(ctx, orDefault(s.PeerTimeout, 300*time.Millisecond))
	defer cancel()

	type answer struct {
		from  netip.AddrPort
		addrs []netip.Addr
	}

	answers := make(chan answer, len(peers))

	for _, p := range peers {
		go func() {
			addrs, _ := queryA(ctx, p, name)
			answers <- answer{from: p, addrs: addrs}
		}()
	}

	var (
		merged []netip.Addr
		owners []string
	)

	for range peers {
		a := <-answers
		if len(a.addrs) == 0 {
			continue
		}

		owners = append(owners, a.from.Addr().String())

		for _, addr := range a.addrs {
			if !slices.Contains(merged, addr) {
				merged = append(merged, addr)
			}
		}
	}

	if len(owners) > 1 {
		slices.Sort(owners)
		s.logf("meshdns: %s is answered by %d hosts (%s) — returning all of them", strings.TrimSuffix(name, "."), len(owners), strings.Join(owners, ", "))
	}

	s.toCache(name, merged)

	return merged
}

// forward passes a name outside the mesh to the host's resolvers, in
// order, over the transport the question came in on, and returns the first
// answer as it came — truncation included, so the client retries over TCP
// exactly as it would against the upstream itself.
func (s *Server) forward(ctx context.Context, hdr dnsmessage.Header, q *dnsmessage.Question, req []byte, tcp bool) []byte {
	for _, up := range s.Upstreams {
		ctx, cancel := context.WithTimeout(ctx, orDefault(s.UpstreamTimeout, 2*time.Second))
		resp, err := exchange(ctx, up, req, tcp)
		cancel()

		if err == nil {
			return resp
		}
	}

	return reply(hdr, q, dnsmessage.RCodeServerFailure, nil, tcp)
}

func (s *Server) fromCache(name string) ([]netip.Addr, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	c, ok := s.cache[name]
	if !ok || !s.clock().Before(c.until) {
		return nil, false
	}

	return c.addrs, true
}

func (s *Server) toCache(name string, addrs []netip.Addr) {
	ttl := answerTTL * time.Second
	if len(addrs) == 0 {
		ttl = negativeTTL
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cache == nil {
		s.cache = map[string]cached{}
	}

	now := s.clock()

	// Expired entries go on every write: names a container asked once and
	// never again must not accumulate.
	for k, c := range s.cache {
		if !now.Before(c.until) {
			delete(s.cache, k)
		}
	}

	s.cache[name] = cached{addrs: addrs, until: now.Add(ttl)}
}

func (s *Server) clock() time.Time {
	if s.now != nil {
		return s.now()
	}

	return time.Now()
}

func (s *Server) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

// ListenAndServe answers on addr over UDP and TCP until ctx ends.
func (s *Server) ListenAndServe(ctx context.Context, addr netip.AddrPort) error {
	pc, err := net.ListenUDP("udp", net.UDPAddrFromAddrPort(addr))
	if err != nil {
		return err
	}

	ln, err := net.ListenTCP("tcp", net.TCPAddrFromAddrPort(addr))
	if err != nil {
		pc.Close()

		return err
	}

	return s.serveOn(ctx, pc, ln)
}

// serveOn answers on listeners already open, until ctx ends.
func (s *Server) serveOn(ctx context.Context, pc *net.UDPConn, ln *net.TCPListener) error {
	go func() {
		<-ctx.Done()
		pc.Close()
		ln.Close()
	}()

	go s.serveTCP(ctx, ln)

	return s.serveUDP(ctx, pc)
}

func (s *Server) serveUDP(ctx context.Context, pc *net.UDPConn) error {
	buf := make([]byte, 65535)

	for {
		n, from, err := pc.ReadFromUDPAddrPort(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}

			return err
		}

		req := append([]byte(nil), buf[:n]...)

		go func() {
			if resp := s.Handle(ctx, from.Addr(), req, false); resp != nil {
				_, _ = pc.WriteToUDPAddrPort(resp, from)
			}
		}()
	}
}

func (s *Server) serveTCP(ctx context.Context, ln *net.TCPListener) {
	for {
		conn, err := ln.AcceptTCP()
		if err != nil {
			return
		}

		go s.serveConn(ctx, conn)
	}
}

func (s *Server) serveConn(ctx context.Context, conn *net.TCPConn) {
	defer conn.Close()

	src := conn.RemoteAddr().(*net.TCPAddr).AddrPort().Addr()

	for {
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

		req, err := readTCPMessage(conn)
		if err != nil {
			return
		}

		resp := s.Handle(ctx, src, req, true)
		if resp == nil || writeTCPMessage(conn, resp) != nil {
			return
		}
	}
}

// queryA asks server for name's A records.
func queryA(ctx context.Context, server netip.AddrPort, name string) ([]netip.Addr, error) {
	qname, err := dnsmessage.NewName(name)
	if err != nil {
		return nil, err
	}

	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: uint16(rand.Uint32())})
	_ = b.StartQuestions()
	_ = b.Question(dnsmessage.Question{Name: qname, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET})

	req, err := b.Finish()
	if err != nil {
		return nil, err
	}

	resp, err := exchange(ctx, server, req, false)
	if err != nil {
		return nil, err
	}

	if truncated(resp) {
		if resp, err = exchange(ctx, server, req, true); err != nil {
			return nil, err
		}
	}

	return answersA(resp)
}

func answersA(resp []byte) ([]netip.Addr, error) {
	var p dnsmessage.Parser

	hdr, err := p.Start(resp)
	if err != nil {
		return nil, err
	}

	if hdr.RCode != dnsmessage.RCodeSuccess {
		return nil, nil
	}

	if err := p.SkipAllQuestions(); err != nil {
		return nil, err
	}

	var out []netip.Addr

	for {
		h, err := p.AnswerHeader()
		if err != nil {
			break
		}

		if h.Type != dnsmessage.TypeA {
			_ = p.SkipAnswer()

			continue
		}

		a, err := p.AResource()
		if err != nil {
			return nil, err
		}

		out = append(out, netip.AddrFrom4(a.A))
	}

	return out, nil
}

func truncated(resp []byte) bool {
	var p dnsmessage.Parser

	hdr, err := p.Start(resp)

	return err == nil && hdr.Truncated
}

// exchange sends msg to server and returns the reply with the same id.
func exchange(ctx context.Context, server netip.AddrPort, msg []byte, tcp bool) ([]byte, error) {
	network := "udp"
	if tcp {
		network = "tcp"
	}

	var d net.Dialer

	conn, err := d.DialContext(ctx, network, server.String())
	if err != nil {
		return nil, err
	}

	defer conn.Close()

	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	if tcp {
		if err := writeTCPMessage(conn, msg); err != nil {
			return nil, err
		}

		return readTCPMessage(conn)
	}

	if _, err := conn.Write(msg); err != nil {
		return nil, err
	}

	buf := make([]byte, 65535)

	for {
		n, err := conn.Read(buf)
		if err != nil {
			return nil, err
		}

		// A stray datagram with another id is not our answer.
		if n >= 2 && buf[0] == msg[0] && buf[1] == msg[1] {
			return append([]byte(nil), buf[:n]...), nil
		}
	}
}

func readTCPMessage(r io.Reader) ([]byte, error) {
	var size [2]byte

	if _, err := io.ReadFull(r, size[:]); err != nil {
		return nil, err
	}

	msg := make([]byte, binary.BigEndian.Uint16(size[:]))

	if _, err := io.ReadFull(r, msg); err != nil {
		return nil, err
	}

	return msg, nil
}

func writeTCPMessage(w io.Writer, msg []byte) error {
	out := make([]byte, 2+len(msg))
	binary.BigEndian.PutUint16(out, uint16(len(msg)))
	copy(out[2:], msg)

	_, err := w.Write(out)

	return err
}

// reply builds the answer to req. Over UDP an answer past the classic
// limit goes out truncated and empty; the client asks again over TCP.
func reply(req dnsmessage.Header, q *dnsmessage.Question, rcode dnsmessage.RCode, addrs []netip.Addr, tcp bool) []byte {
	msg := build(req, q, rcode, addrs, false)

	if !tcp && len(msg) > maxUDPResponse {
		msg = build(req, q, rcode, nil, true)
	}

	return msg
}

func build(req dnsmessage.Header, q *dnsmessage.Question, rcode dnsmessage.RCode, addrs []netip.Addr, truncate bool) []byte {
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{
		ID:                 req.ID,
		Response:           true,
		OpCode:             req.OpCode,
		Authoritative:      q != nil && strings.HasSuffix(strings.ToLower(q.Name.String()), ".voodu."),
		Truncated:          truncate,
		RecursionDesired:   req.RecursionDesired,
		RecursionAvailable: true,
		RCode:              rcode,
	})
	b.EnableCompression()

	if q != nil {
		_ = b.StartQuestions()
		_ = b.Question(*q)
	}

	if len(addrs) > 0 {
		_ = b.StartAnswers()

		for _, a := range addrs {
			_ = b.AResource(
				dnsmessage.ResourceHeader{Name: q.Name, Class: dnsmessage.ClassINET, TTL: answerTTL},
				dnsmessage.AResource{A: a.As4()},
			)
		}
	}

	msg, _ := b.Finish()

	return msg
}

// shuffled returns addrs in a random order, so clients that take the first
// address spread across replicas the way docker's own round robin does.
func shuffled(addrs []netip.Addr) []netip.Addr {
	out := slices.Clone(addrs)
	rand.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })

	return out
}

func orDefault(d, fallback time.Duration) time.Duration {
	if d > 0 {
		return d
	}

	return fallback
}
