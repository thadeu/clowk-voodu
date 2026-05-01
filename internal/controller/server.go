package controller

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"go.voodu.clowk.in/internal/envfile"
	"go.voodu.clowk.in/internal/paths"
	"go.voodu.clowk.in/internal/plugins"
	"go.voodu.clowk.in/internal/secrets"
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
	PluginsRoot  string // /opt/voodu/plugins
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

	cronSched   *CronScheduler
	cancelCron  context.CancelFunc
	cronDone    chan struct{}
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

	recCtx, cancel := context.WithCancel(context.Background())
	s.cancelRec = cancel

	// Single invoker shared by /exec and the reconciler's handlers —
	// one env-injection path, one plugin-resolution path.
	invoker := &DirInvoker{
		PluginsRoot: s.cfg.PluginsRoot,
		NodeName:    s.cfg.NodeName,
		EtcdClient:  s.cfg.EtcdClient,
	}

	s.api = &API{
		Store:       store,
		Version:     s.cfg.Version,
		PluginsRoot:   s.cfg.PluginsRoot,
		NodeName:      s.cfg.NodeName,
		EtcdClient:    s.cfg.EtcdClient,
		ControllerURL: deriveControllerURL(s.cfg.HTTPAddr),
		Invoker:       invoker,
		Pods:        DockerPodsLister{},
		// DockerContainerManager satisfies LogStreamer via its Logs
		// method — same instance the deployment/job/cronjob handlers
		// already use, so /pods/{name}/logs and the runners agree on
		// docker access.
		Logs: DockerContainerManager{},

		// Same instance — its Exec method satisfies the Execer seam.
		Execer: DockerContainerManager{},

		// Plugin-block expansion: discovers plugins under
		// PluginsRoot at apply time and routes non-core kinds
		// through their `expand` command. M-D2+ kicks in.
		PluginBlocks: &DirPluginRegistry{PluginsRoot: s.cfg.PluginsRoot},

		// JIT-install plugins missing at apply time. Convention
		// repo is `thadeu/voodu-<kind>`; operators override per
		// block via `_repo` attribute or per-server via env.
		PluginInstaller: &plugins.Installer{Root: s.cfg.PluginsRoot},
	}

	depHandler := &DeploymentHandler{
		Store: store,
		Log:   s.cfg.Logger,
		WriteEnv: func(app string, pairs []string) (bool, error) {
			envFile := paths.AppEnvFile(app)
			// Load pre-image first so we can diff against the merged
			// result and decide whether to restart the container. A
			// missing file is fine — treat as empty.
			before, _ := envfile.Load(envFile)

			// Replace, not Set — the reconciler's pairs are the
			// COMPLETE merged state (config bucket + spec.env).
			// Set's overlay semantics would leave keys removed
			// from the bucket lingering in the .env file forever.
			after, err := secrets.Replace(app, pairs)
			if err != nil {
				return false, err
			}

			return !stringMapsEqual(before, after), nil
		},
		EnvFilePath: paths.AppEnvFile,
		Containers:  DockerContainerManager{},
	}

	assetHandler := &AssetHandler{
		Store: store,
		Log:   s.cfg.Logger,
	}

	stsHandler := &StatefulsetHandler{
		Store: store,
		Log:   s.cfg.Logger,
		WriteEnv: func(app string, pairs []string) (bool, error) {
			envFile := paths.AppEnvFile(app)
			before, _ := envfile.Load(envFile)

			// Replace semantics — see deployment WriteEnv for why.
			after, err := secrets.Replace(app, pairs)
			if err != nil {
				return false, err
			}

			return !stringMapsEqual(before, after), nil
		},
		EnvFilePath: paths.AppEnvFile,
		Containers:  DockerContainerManager{},
	}

	ingHandler := &IngressHandler{
		Store:      store,
		Invoker:    invoker,
		Log:        s.cfg.Logger,
		Containers: DockerContainerManager{},
		// PluginName left empty → defaults to "caddy". Operators with a
		// non-Caddy router install their own plugin and set this via a
		// future Config field.
	}

	jobHandler := &JobHandler{
		Store:      store,
		Log:        s.cfg.Logger,
		Containers: DockerContainerManager{},
		// Jobs read the same env file as their AppID-twinned deployment
		// would (apps/<scope>-<name>/.env), so `voodu config set` lands
		// where job runs read from.
		EnvFilePath: paths.AppEnvFile,
		WriteEnv: func(app string, pairs []string) (bool, error) {
			envFile := paths.AppEnvFile(app)

			before, _ := envfile.Load(envFile)

			// Replace semantics — see deployment WriteEnv for why.
			after, err := secrets.Replace(app, pairs)
			if err != nil {
				return false, err
			}

			return !stringMapsEqual(before, after), nil
		},
	}

	// Expose the job runner to the API so `voodu run job` has a target
	// for /jobs/run. The reconciler still sees jobHandler.Handle for
	// apply / delete events.
	s.api.Jobs = jobHandler

	// Same instance for the imperative restart path.
	s.api.Deployments = depHandler

	// Statefulset surface mirrors deployment for the
	// imperative restart / rollback / prune-volumes paths.
	s.api.Statefulsets = stsHandler

	cronJobHandler := &CronJobHandler{
		Store:       store,
		Log:         s.cfg.Logger,
		Containers:  DockerContainerManager{},
		EnvFilePath: paths.AppEnvFile,
		WriteEnv: func(app string, pairs []string) (bool, error) {
			envFile := paths.AppEnvFile(app)

			before, _ := envfile.Load(envFile)

			// Replace semantics — see deployment WriteEnv for why.
			after, err := secrets.Replace(app, pairs)
			if err != nil {
				return false, err
			}

			return !stringMapsEqual(before, after), nil
		},
	}

	// Expose the cronjob runner to the API so `voodu run cronjob` has
	// a target for /cronjobs/run. The scheduler still sees
	// cronJobHandler directly for scheduled tick dispatches; this is
	// purely the imperative "run now" path.
	s.api.CronJobs = cronJobHandler

	s.rec = &Reconciler{
		Store:  store,
		Logger: s.cfg.Logger,
		Handlers: map[Kind]HandlerFunc{
			KindDeployment:  depHandler.Handle,
			KindStatefulset: stsHandler.Handle,
			KindIngress:     ingHandler.Handle,
			KindJob:         jobHandler.Handle,
			KindCronJob:     cronJobHandler.Handle,
			KindAsset:       assetHandler.Handle,
		},
	}

	// Wall-clock dispatcher. Lives on its own goroutine so a slow tick
	// can't block reconciles, and so Stop() can shut it down
	// independently. Interval defaults to 1m (cron resolution); Now and
	// Dispatch take their production defaults — tests inject mocks.
	s.cronSched = &CronScheduler{
		Store:   store,
		Handler: cronJobHandler,
		Logger:  s.cfg.Logger,
	}

	s.recDone = make(chan struct{})

	go func() {
		defer close(s.recDone)

		if err := s.rec.Run(recCtx); err != nil {
			s.cfg.Logger.Printf("reconciler exited: %v", err)
		}
	}()

	cronCtx, cancelCron := context.WithCancel(context.Background())
	s.cancelCron = cancelCron
	s.cronDone = make(chan struct{})

	go func() {
		defer close(s.cronDone)

		s.cronSched.Run(cronCtx)
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
	if s.cancelCron != nil {
		s.cancelCron()
	}

	if s.cronDone != nil {
		<-s.cronDone
	}

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

// Hijack forwards the http.Hijacker call to the wrapped writer when
// the underlying connection supports it. /pods/{name}/exec relies on
// hijack to take over the conn for bidirectional streaming; without
// this method the embedded ResponseWriter would mask the Hijacker
// interface and the exec path would 500 with "hijack not supported"
// even though net/http natively supports it.
func (l *loggingResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := l.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("underlying ResponseWriter does not support Hijack")
	}

	return hijacker.Hijack()
}

// Flush forwards the http.Flusher call. Needed for /pods/{name}/logs
// streaming so chunked transfer pushes lines to the client without
// waiting for the page boundary. Same masking issue as Hijack above.
func (l *loggingResponseWriter) Flush() {
	if flusher, ok := l.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// deriveControllerURL turns an HTTPAddr like ":8686" or
// "0.0.0.0:8686" into the URL plugins use to call back into the
// controller. Plugins run on the same host as the controller
// (server-side install via PluginsRoot) so 127.0.0.1 is the
// right host regardless of what the listener binds to.
//
// Empty addr → empty URL (plugins won't be able to call back,
// but voodu-redis-style plugins that need state still work
// when invoked through the dispatch endpoint with stdin
// envelope already populated).
func deriveControllerURL(httpAddr string) string {
	if httpAddr == "" {
		return ""
	}

	// HTTPAddr can be ":8686" (any iface) or "host:8686" or
	// "1.2.3.4:8686". Plugins run locally — always loopback.
	port := httpAddr
	if idx := strings.LastIndex(httpAddr, ":"); idx >= 0 {
		port = httpAddr[idx:]
	}

	return "http://127.0.0.1" + port
}

