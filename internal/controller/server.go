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

	"go.voodu.clowk.in/internal/docker"
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
	HTTPAddr     string // 127.0.0.1:8686 — orchestration plane (CLI via SSH)
	EtcdClient   string // http://127.0.0.1:2379
	EtcdPeer     string // http://127.0.0.1:2380
	NodeName     string // voodu-0
	PluginsRoot  string // /opt/voodu/plugins
	Version      string
	Logger       *log.Logger
	QuietEtcd    bool
	ReadyTimeout time.Duration // default 30s

	// PATAddr is the bind address for the PAT-authenticated
	// observability plane (`/api/pat/v1/*`). Defaults to
	// `0.0.0.0:8687` — operator firewalls this port to the WebUI
	// host IP. Empty string disables the plane entirely (no second
	// listener spawned, no PAT routes exposed).
	PATAddr string

	// PATActionRate is the per-PAT refill rate (tokens per second)
	// for action endpoints (today: restart). Default 10/60 = ~0.166
	// tokens/sec = 10 actions/min steady-state. Zero/negative
	// disables rate limiting entirely (escape hatch for dev envs).
	PATActionRate float64

	// PATActionBurst is the per-PAT bucket capacity for action
	// endpoints. Default 3 — operator can quickly chain up to 3
	// restarts before steady-state kicks in.
	PATActionBurst int
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

	// httpPAT is the second HTTP listener serving `/api/pat/v1/*`.
	// nil when Config.PATAddr is empty (PAT plane disabled).
	httpPAT *http.Server

	cancelRec context.CancelFunc
	recDone   chan struct{}

	cronSched   *CronScheduler
	cancelCron  context.CancelFunc
	cronDone    chan struct{}

	// Autoscaler goroutine — M7 CPU-driven horizontal scaler for
	// deployments with an `autoscale {}` block. On its own goroutine
	// so a slow stats tick can't block the reconciler; shut down
	// independently in teardown.
	autoscaler     *Autoscaler
	cancelScaler   context.CancelFunc
	autoscalerDone chan struct{}
}

func NewServer(cfg Config) *Server {
	if cfg.HTTPAddr == "" {
		// Default narrowed to 127.0.0.1 in C5 of the PAT plan —
		// orchestration plane is localhost-only by default; the
		// observability plane (PATAddr) carries the network-
		// exposable surface.
		cfg.HTTPAddr = "127.0.0.1:8686"
	}

	if cfg.PATActionRate == 0 {
		// 10 actions per minute = 1/6 tokens/sec.
		cfg.PATActionRate = 10.0 / 60.0
	}

	if cfg.PATActionBurst == 0 {
		cfg.PATActionBurst = 3
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

		// Same instance — its Stop / Start / InspectLabels methods
		// satisfy the PodLifecycler seam used by `vd stop` /
		// `vd start`.
		PodLifecycle: DockerContainerManager{},

		// Stats collector — wires the existing pods lister + a
		// fresh DockerStatsClient + the same Store everything else
		// uses. Single shape powers /stats today; future SDK will
		// re-export the typed result without re-implementing the
		// join.
		Stats: &StatsCollector{
			Pods:  DockerPodsLister{},
			Stats: docker.DockerStatsClient{},
			Store: store,
		},

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
		// Probe registry — wires the kubelet-style liveness runners
		// to docker restart + IP resolution + exec. Same docker
		// surface the rest of the handler uses; tests substitute
		// fakes per field.
		Probes: &ProbeRegistry{
			Restart: dockerRestarter{},
			IPs:     dockerIPResolver{},
			Exec:    DockerContainerManager{},
			Log:     s.cfg.Logger,
		},
	}

	assetHandler := &AssetHandler{
		Store: store,
		Log:   s.cfg.Logger,
	}

	// Registry handler owns ~/.docker/config.json on the host: each
	// `registry "name" { … }` manifest becomes one entry under
	// `auths`, regenerated atomically on every Put / Delete watch
	// event. DockerConfigPath stays empty so the handler resolves
	// $HOME/.docker/config.json at reconcile time — production
	// runs the controller as the same user that shells out to
	// `docker pull`, so one HOME == one config file.
	registryHandler := &RegistryHandler{
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
		Probes: &ProbeRegistry{
			Restart: dockerRestarter{},
			IPs:     dockerIPResolver{},
			Exec:    DockerContainerManager{},
			Log:     s.cfg.Logger,
		},
		// Container-side URL (host.docker.internal-based) — this
		// flows into pod env via BuildPlatformEnv. Different from
		// API.ControllerURL above (which is 127.0.0.1-based for
		// host-process plugin callbacks).
		ControllerURL: deriveContainerControllerURL(s.cfg.HTTPAddr),
	}

	// Recorder back-references complete the M1.2 / M1.3 wiring:
	// every readiness/startup phase transition flows back into
	// the resource's status blob via the handler's
	// RecordReplicaReadiness / ClearReplicaReadiness. Self-
	// reference breaks the construction chicken-and-egg without
	// inverting ownership.
	depHandler.Probes.Recorder = depHandler
	stsHandler.Probes.Recorder = stsHandler

	// API.Readiness is the in-memory lookup `GET /pods/{name}/ready`
	// reads from. Composite over both registries so the same
	// endpoint serves deployment AND statefulset replicas — caddy
	// (and operators) don't have to know which kind owns a given
	// container.
	s.api.Readiness = compositeReadinessLookup{depHandler.Probes, stsHandler.Probes}

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
			KindRegistry:    registryHandler.Handle,
		},
		// Persist reconcile errors on the per-kind status blob so
		// `vd describe` shows WHY a deployment is stuck (and
		// `vd apply`'s post-apply polling can surface it). Only
		// kinds that carry a DeploymentStatus blob participate;
		// for other kinds the call is a no-op.
		OnReconcile: func(ev WatchEvent, reconcileErr error) {
			recordReconcileResult(context.Background(), store, ev, reconcileErr, s.cfg.Logger)
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

	// Autoscaler — reuses the same StatsCollector that powers the
	// /stats API and `vd stats`, so scaler decisions come from the
	// exact same runtime numbers operators see. Writes back to the
	// store via StoreReplicasApplier; the reconciler picks the spec
	// change up through the regular watch path. No separate runtime.
	scalerCtx, cancelScaler := context.WithCancel(context.Background())
	s.cancelScaler = cancelScaler
	s.autoscalerDone = make(chan struct{})

	s.autoscaler = &Autoscaler{
		Store:  store,
		Stats:  s.api.Stats,
		Apply:  StoreReplicasApplier{Store: store},
		Logger: s.cfg.Logger,
	}

	go func() {
		defer close(s.autoscalerDone)

		s.autoscaler.Run(scalerCtx)
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

	// PAT (observability) plane — second listener on a separate
	// port. Network-exposable; gated by PAT auth + rate limit.
	// Empty PATAddr disables the plane entirely (no second
	// listener, no PAT routes).
	if s.cfg.PATAddr != "" {
		patListener, perr := listenOn(s.cfg.PATAddr)
		if perr != nil {
			s.teardown()
			return fmt.Errorf("listen PAT %s: %w", s.cfg.PATAddr, perr)
		}

		s.httpPAT = &http.Server{
			Addr:              patListener.Addr().String(),
			Handler:           s.api.PATHandler(s.cfg.Logger, s.cfg.PATActionRate, s.cfg.PATActionBurst),
			ReadHeaderTimeout: 5 * time.Second,
		}

		s.cfg.Logger.Printf("PAT plane listening on %s (rate=%.3f/sec, burst=%d)",
			s.httpPAT.Addr, s.cfg.PATActionRate, s.cfg.PATActionBurst)

		go func() {
			if err := s.httpPAT.Serve(patListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				s.cfg.Logger.Printf("PAT http server exited: %v", err)
			}
		}()
	}

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

// Stop shuts down the HTTP listener(s), stops the reconciler, and
// closes etcd. Blocks until all goroutines exit or timeout elapses.
func (s *Server) Stop(timeout time.Duration) error {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	if s.http != nil {
		_ = s.http.Shutdown(shutdownCtx)
	}

	if s.httpPAT != nil {
		_ = s.httpPAT.Shutdown(shutdownCtx)
	}

	s.teardown()

	return nil
}

// PATAddr returns the actual PAT-plane listen address (useful in
// tests where the caller asked for :0 to pick a free port).
// Empty when the PAT plane is disabled.
func (s *Server) PATAddr() string {
	if s.httpPAT == nil {
		return ""
	}

	return s.httpPAT.Addr
}

func (s *Server) teardown() {
	if s.cancelScaler != nil {
		s.cancelScaler()
	}

	if s.autoscalerDone != nil {
		<-s.autoscalerDone
	}

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
// "0.0.0.0:8686" into the URL HOST-PROCESS plugins use to call
// back into the controller. Plugin processes are forked by the
// controller during dispatch and run on the same host — 127.0.0.1
// always works.
//
// Empty addr → empty URL (plugins won't be able to call back,
// but voodu-redis-style plugins that need state still work
// when invoked through the dispatch endpoint with stdin envelope
// already populated).
//
// For CONTAINER env injection (sentinel pods + future plugins
// that call back from inside docker), use deriveContainerControllerURL
// — that one returns a host.docker.internal-based URL so the call
// crosses the docker bridge correctly.
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

// deriveContainerControllerURL is the container-side peer of
// deriveControllerURL. Returns host.docker.internal:<port> so
// in-container scripts (sentinel failover hook, M-S4 preflight,
// future probe-style plugins) reach the controller across the
// docker bridge.
//
// Pairs with the --add-host host.docker.internal:host-gateway
// flag voodu passes on every container in CreateContainer —
// that flag makes the alias resolvable on Linux (Docker Desktop
// already provides it natively on Mac/Win).
//
// IMPORTANT: the controller MUST bind to an address reachable
// from the docker bridge gateway (0.0.0.0:<port> or the bridge
// IP). A controller bound to 127.0.0.1:<port> won't accept the
// connection arriving from the gateway, even though the alias
// resolves correctly. The controller's --http flag default is
// `:8686` which binds all interfaces; setups overriding to
// 127.0.0.1:8686 break in-container callbacks.
//
// Empty addr → empty URL (containers detect and skip the
// callback path; sentinel hook degrades gracefully).
func deriveContainerControllerURL(httpAddr string) string {
	if httpAddr == "" {
		return ""
	}

	port := httpAddr
	if idx := strings.LastIndex(httpAddr, ":"); idx >= 0 {
		port = httpAddr[idx:]
	}

	return "http://host.docker.internal" + port
}

