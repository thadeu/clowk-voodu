package controller

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"
)

// Config is everything the controller needs to start. All fields have
// working defaults except DataDir, which must point at the etcd state
// directory (normally /opt/voodu/state).
type Config struct {
	DataDir      string
	HTTPAddr     string // :8686
	EtcdClient   string // http://127.0.0.1:2379
	EtcdPeer     string // http://127.0.0.1:2380
	NodeName     string // voodu-0
	Version      string
	Logger       *log.Logger
	QuietEtcd    bool
	ReadyTimeout time.Duration // default 30s
}

// Server composes embedded etcd + HTTP API + reconciler into a single
// lifecycle. Start returns once everything is listening; Stop blocks
// until the shutdown is complete.
type Server struct {
	cfg  Config
	etcd *EmbeddedEtcd
	api  *API
	rec  *Reconciler
	http *http.Server

	cancelRec context.CancelFunc
	recDone   chan struct{}
}

func NewServer(cfg Config) *Server {
	if cfg.HTTPAddr == "" {
		cfg.HTTPAddr = ":8686"
	}

	if cfg.NodeName == "" {
		cfg.NodeName = "voodu-0"
	}

	if cfg.Logger == nil {
		cfg.Logger = log.Default()
	}

	if cfg.ReadyTimeout == 0 {
		cfg.ReadyTimeout = 30 * time.Second
	}

	return &Server{cfg: cfg}
}

// Start boots etcd, wires the API, starts the HTTP listener and
// launches the reconciler goroutine. Returns the first error that
// prevents the server from becoming ready.
func (s *Server) Start(ctx context.Context) error {
	if s.cfg.DataDir == "" {
		return errors.New("controller: DataDir is required")
	}

	etcd, err := StartEmbeddedEtcd(EtcdConfig{
		Name:      s.cfg.NodeName,
		DataDir:   s.cfg.DataDir,
		ClientURL: s.cfg.EtcdClient,
		PeerURL:   s.cfg.EtcdPeer,
		Quiet:     s.cfg.QuietEtcd,
	})
	if err != nil {
		return fmt.Errorf("start etcd: %w", err)
	}

	s.etcd = etcd

	store := NewEtcdStore(etcd.Client)

	s.api = &API{Store: store, Version: s.cfg.Version}

	recCtx, cancel := context.WithCancel(context.Background())
	s.cancelRec = cancel

	s.rec = &Reconciler{
		Store:  store,
		Logger: s.cfg.Logger,
	}

	s.recDone = make(chan struct{})

	go func() {
		defer close(s.recDone)

		if err := s.rec.Run(recCtx); err != nil {
			s.cfg.Logger.Printf("reconciler exited: %v", err)
		}
	}()

	s.http = &http.Server{
		Addr:              s.cfg.HTTPAddr,
		Handler:           s.api.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	listener, err := listenOn(s.cfg.HTTPAddr)
	if err != nil {
		s.teardown()
		return fmt.Errorf("listen %s: %w", s.cfg.HTTPAddr, err)
	}

	s.http.Addr = listener.Addr().String()

	s.cfg.Logger.Printf("controller listening on %s (etcd client=%s, data=%s)",
		s.http.Addr, s.cfg.EtcdClient, s.cfg.DataDir)

	go func() {
		if err := s.http.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.cfg.Logger.Printf("http server exited: %v", err)
		}
	}()

	return nil
}

// HTTPAddr returns the actual listen address (useful in tests where the
// caller asked for :0 to pick a free port).
func (s *Server) HTTPAddr() string {
	if s.http == nil {
		return ""
	}

	return s.http.Addr
}

// Store exposes the underlying store so tests can assert contents
// without going through HTTP.
func (s *Server) Store() Store {
	if s.api == nil {
		return nil
	}

	return s.api.Store
}

// Stop shuts down the HTTP listener, stops the reconciler, and closes
// etcd. Blocks until all goroutines exit or timeout elapses.
func (s *Server) Stop(timeout time.Duration) error {
	if s.http != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()

		_ = s.http.Shutdown(shutdownCtx)
	}

	s.teardown()

	return nil
}

func (s *Server) teardown() {
	if s.cancelRec != nil {
		s.cancelRec()
	}

	if s.recDone != nil {
		<-s.recDone
	}

	if s.etcd != nil {
		s.etcd.Close()
		s.etcd = nil
	}
}

// logRequests is the one middleware we keep — a line per HTTP request so
// the systemd journal is actually useful. Everything else belongs in
// handlers.
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		lrw := &loggingResponseWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(lrw, r)

		log.Printf("%s %s %d %s", r.Method, r.URL.Path, lrw.status, time.Since(start))
	})
}

type loggingResponseWriter struct {
	http.ResponseWriter
	status int
}

func (l *loggingResponseWriter) WriteHeader(code int) {
	l.status = code
	l.ResponseWriter.WriteHeader(code)
}
