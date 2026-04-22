package controller

import "net"

// listenOn is factored out so tests can pass ":0" and discover the port
// via Listener.Addr(). Keeps s.http.Addr honest — Serve updates the
// struct so external callers see the resolved address.
func listenOn(addr string) (net.Listener, error) {
	l, err := net.Listen("tcp", addr)

	if err != nil {
		return nil, err
	}

	return l, nil
}
