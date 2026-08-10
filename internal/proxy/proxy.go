// Package proxy implements the L4 TCP listener that forwards client
// connections to healthy backends. It is a pure passthrough: TLS between
// the client (kubelet, kube-proxy) and the apiserver is never terminated.
package proxy

import (
	"context"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/cooloo9871/local-apiserver-lb/internal/backend"
)

// Options configures a proxy Server.
type Options struct {
	DialTimeout     time.Duration
	KeepalivePeriod time.Duration // 0 disables TCP keepalive
	Logger          *slog.Logger
}

// Server accepts client connections and pipes them to backends.
type Server struct {
	pool *backend.Pool
	opts Options

	mu           sync.Mutex
	listener     net.Listener
	clientConns  map[net.Conn]struct{}
	shuttingDown bool

	wg sync.WaitGroup // in-flight connection handlers
}

// New builds a Server; call Serve to start accepting.
func New(pool *backend.Pool, opts Options) *Server {
	return &Server{
		pool:        pool,
		opts:        opts,
		clientConns: make(map[net.Conn]struct{}),
	}
}

// Serve accepts connections on ln until Shutdown closes it. It returns
// nil on graceful shutdown, or the accept error otherwise.
func (s *Server) Serve(ln net.Listener) error {
	s.mu.Lock()
	s.listener = ln
	down := s.shuttingDown
	s.mu.Unlock()
	if down {
		// Shutdown won the race before the listener was registered.
		ln.Close()
		return nil
	}

	for {
		conn, err := ln.Accept()
		if err != nil {
			s.mu.Lock()
			down := s.shuttingDown
			s.mu.Unlock()
			if down {
				return nil
			}
			return err
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handle(conn)
		}()
	}
}

// Shutdown stops accepting new connections, waits for in-flight
// connections until ctx expires, then force-closes whatever remains.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.shuttingDown = true
	ln := s.listener
	s.mu.Unlock()
	if ln != nil {
		ln.Close()
	}

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
	}

	s.mu.Lock()
	remaining := len(s.clientConns)
	for c := range s.clientConns {
		c.Close()
	}
	s.mu.Unlock()
	if remaining > 0 {
		s.opts.Logger.Warn("shutdown grace expired; force-closed remaining connections",
			"connections", remaining)
	}

	<-done
	return nil
}

// handle proxies one client connection to the first backend that accepts
// a dial, in the order given by the balancing strategy.
func (s *Server) handle(clientConn net.Conn) {
	defer clientConn.Close()
	s.trackClient(clientConn, true)
	defer s.trackClient(clientConn, false)

	s.configureKeepalive(clientConn)

	candidates, degraded := s.pool.Candidates()
	if degraded {
		s.opts.Logger.Debug("no healthy backends; trying all in best-effort mode",
			"client", clientConn.RemoteAddr())
	}

	for _, b := range candidates {
		backendConn, err := net.DialTimeout("tcp", b.Addr(), s.opts.DialTimeout)
		if err != nil {
			b.Stats().DialErrors.Add(1)
			s.opts.Logger.Debug("backend dial failed, trying next candidate",
				"backend", b.Addr(), "error", err)
			continue
		}
		s.configureKeepalive(backendConn)

		tracked := b.Track(backendConn)
		s.opts.Logger.Debug("connection established",
			"client", clientConn.RemoteAddr(), "backend", b.Addr())
		s.pipe(clientConn, tracked)
		s.opts.Logger.Debug("connection closed",
			"client", clientConn.RemoteAddr(), "backend", b.Addr())
		return
	}

	s.opts.Logger.Debug("dropping client connection: no backend reachable",
		"client", clientConn.RemoteAddr(), "candidates", len(candidates))
}

// pipe copies bytes in both directions until both sides are done. A
// one-sided EOF is propagated as a TCP half-close (CloseWrite) so the
// other direction keeps flowing; the connection is only torn down once
// both directions have finished.
func (s *Server) pipe(clientConn, backendConn net.Conn) {
	defer backendConn.Close()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		copyThenCloseWrite(backendConn, clientConn)
	}()
	go func() {
		defer wg.Done()
		copyThenCloseWrite(clientConn, backendConn)
	}()
	wg.Wait()
}

// copyThenCloseWrite copies src to dst until EOF or error, then signals
// end-of-stream to dst without closing its read side.
func copyThenCloseWrite(dst, src net.Conn) {
	io.Copy(dst, src)
	if cw, ok := dst.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
	} else {
		dst.Close()
	}
}

// trackClient registers or unregisters a client connection for
// force-close on shutdown.
func (s *Server) trackClient(c net.Conn, add bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if add {
		s.clientConns[c] = struct{}{}
	} else {
		delete(s.clientConns, c)
	}
}

// configureKeepalive enables TCP keepalive on TCP connections; a zero
// period disables it.
func (s *Server) configureKeepalive(c net.Conn) {
	tcp, ok := c.(*net.TCPConn)
	if !ok {
		return
	}
	if s.opts.KeepalivePeriod <= 0 {
		tcp.SetKeepAlive(false)
		return
	}
	tcp.SetKeepAlive(true)
	tcp.SetKeepAlivePeriod(s.opts.KeepalivePeriod)
}
