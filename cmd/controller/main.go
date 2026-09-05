package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"encoding/json"
	"go.voodu.clowk.in/internal/activity"
	"go.voodu.clowk.in/internal/controller"
	"go.voodu.clowk.in/internal/deploy"
	"go.voodu.clowk.in/internal/docker"
	"go.voodu.clowk.in/internal/manifest"
	"go.voodu.clowk.in/internal/metrics"
	"go.voodu.clowk.in/internal/paths"
	"io"
)

var (
	version = "0.1.0-dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	// Defaults are *secure by default*: the orchestration plane binds
	// to loopback only, the observability plane binds to all interfaces
	// (so the WebUI can reach it across the network — operator firewalls
	// to the WebUI host IP).
	//
	// Both flags honour matching env vars so the systemd unit can
	// override without flag noise. Precedence: flag > env > default.
	var (
		httpAddr    = flag.String("http", envOr("VOODU_HTTP_ADDR", "127.0.0.1:8686"), "HTTP API listen address (orchestration plane; env: VOODU_HTTP_ADDR)")
		patAddr     = flag.String("pat-addr", envOr("VOODU_PAT_ADDR", "0.0.0.0:8687"), "PAT-authenticated observability plane listen address; empty disables (env: VOODU_PAT_ADDR)")
		patRate     = flag.Float64("pat-action-rate", 10.0/60.0, "per-PAT action requests-per-second steady rate (default: 10/min)")
		patBurst    = flag.Int("pat-action-burst", 3, "per-PAT action burst size")
		etcdClient  = flag.String("etcd-client", "http://127.0.0.1:2379", "etcd client URL")
		etcdPeer    = flag.String("etcd-peer", "http://127.0.0.1:2380", "etcd peer URL")
		dataDir     = flag.String("data", "", "etcd data directory (default: <VOODU_ROOT>/state)")
		pluginsDir  = flag.String("plugins", "", "plugin root directory (default: <VOODU_ROOT>/plugins)")
		buildRoot   = flag.String("build-root", "", "where deploys unpack a repository (default: <data>/builds)")
		nodeName    = flag.String("name", "voodu-0", "etcd cluster member name")
		quietEtcd   = flag.Bool("quiet-etcd", true, "suppress etcd info logging")
		showVersion = flag.Bool("version", false, "print version and exit")

		// Metrics sampler — persists 15s-cadence time-series rows to
		// NDJSON under `<VOODU_ROOT>/cache/metrics/` so WebUI charts
		// can render history. Both honour env vars so the systemd unit
		// can tune retention/cadence per-host without changing the
		// service file's ExecStart.
		metricsInterval  = flag.Duration("metrics-interval", parseDurationOr("VOODU_METRICS_INTERVAL", metrics.DefaultInterval), "metrics sampler tick cadence (env: VOODU_METRICS_INTERVAL, default 15s)")
		metricsRetention = flag.Duration("metrics-retention", parseDurationOr("VOODU_METRICS_RETENTION", metrics.DefaultRetention), "metrics file retention window (env: VOODU_METRICS_RETENTION, default 168h = 7d)")

		// Activity trail — the append-only record of operator actions
		// under `<VOODU_ROOT>/state/activity/`. Only retention is
		// tunable: there is no cadence, the handlers drive the writes.
		activityRetention = flag.Duration("activity-retention", parseDurationOr("VOODU_ACTIVITY_RETENTION", activity.DefaultRetention), "activity trail retention window (env: VOODU_ACTIVITY_RETENTION, default 720h = 30d)")

		// Ingress sampler — tails voodu-caddy's JSON access log,
		// aggregates per-deployment HTTP metrics (count, status
		// breakdown, latency p50/p90/p95/p99) in the same Tick cadence
		// as the system+pod sampler. Default path matches voodu-caddy's
		// install: $VOODU_CADDY_STATE_DIR/logs/access.log bind-mounted
		// to /var/log/caddy inside the Caddy container.
		caddyLog = flag.String("caddy-log", envOr("VOODU_CADDY_LOG", metrics.DefaultCaddyAccessLog), "voodu-caddy access log path; empty disables ingress sampler (env: VOODU_CADDY_LOG)")
	)

	flag.Parse()

	if *showVersion {
		fmt.Printf("voodu-controller %s (commit: %s, built: %s)\n", version, commit, date)
		return
	}

	if *dataDir == "" {
		*dataDir = paths.StateDir()
	}

	if *pluginsDir == "" {
		*pluginsDir = paths.PluginsDir()
	}

	logger := log.New(os.Stderr, "", log.LstdFlags|log.Lmsgprefix)

	// Point this process — and every `docker` it forks — at
	// <VOODU_ROOT>/docker/config.json before anything can pull. Done
	// here rather than inside the server so the env var is set once,
	// by the process owner, ahead of the reconciler's first replay.
	//
	// Non-fatal: a controller that cannot prepare the directory still
	// reconciles everything that does not need a private registry, and
	// the log line tells the operator exactly why the pulls that do
	// need one are failing.
	if dir, seeded, err := docker.UseVooduDockerConfig(); err != nil {
		logger.Printf("docker config: %v — falling back to docker's default; `registry` manifests will not authenticate pulls", err)
	} else {
		logger.Printf("docker config at %s", dir)

		if seeded {
			logger.Printf("docker config seeded from $HOME/.docker/config.json")
		}
	}

	srv := controller.NewServer(controller.Config{
		DataDir:           *dataDir,
		HTTPAddr:          *httpAddr,
		PATAddr:           *patAddr,
		PATActionRate:     *patRate,
		PATActionBurst:    *patBurst,
		EtcdClient:        *etcdClient,
		EtcdPeer:          *etcdPeer,
		NodeName:          *nodeName,
		PluginsRoot:       *pluginsDir,
		BuildRoot:         *buildRoot,
		Version:           fmt.Sprintf("%s (commit: %s)", version, commit),
		Logger:            logger,
		QuietEtcd:         *quietEtcd,
		MetricsInterval:   *metricsInterval,
		MetricsRetention:  *metricsRetention,
		ActivityRetention: *activityRetention,

		// The deploy executor parses the manifest file a repository's trigger
		// names. Wired HERE and not inside the controller because
		// internal/manifest imports internal/controller for the Manifest
		// type — main is the only place that can see both.
		ParseManifests: func(r io.Reader, format string, vars map[string]string) ([]controller.Manifest, error) {
			return manifest.ParseReader(r, manifest.Format(format), vars)
		},

		// Build-mode deploys. Wired here for the same reason as the parser:
		// internal/deploy imports internal/controller, so the dependency
		// cannot run both ways. The spec arrives as raw JSON because only
		// this file can name deploy.Spec.
		BuildFromSource: func(app string, src io.Reader, buildSpec json.RawMessage, force bool) error {
			var spec *deploy.Spec

			if len(buildSpec) > 0 {
				spec = &deploy.Spec{}

				if err := json.Unmarshal(buildSpec, spec); err != nil {
					// A spec that will not decode is not a reason to refuse
					// the build: the pipeline falls back to auto-detection,
					// which is what a manifest with a bare `build {}` gets.
					spec = nil
				}
			}

			return deploy.RunFromTarball(app, src, deploy.Options{
				Spec:  spec,
				Force: force,

				// Beside the state this box already writes, never /tmp — a
				// hardened unit or a read-only rootfs makes that unwritable,
				// and the deploy fails for a reason that has nothing to do
				// with the deploy. Same rule as --build-root.
				ScratchDir: scratchDir(*buildRoot),

				// GitHub wraps the tree in `owner-repo-<sha>/`, but the
				// controller already stripped that when it extracted and
				// re-tarred each build context — so what arrives here is
				// rooted at the context, exactly like a CLI push.
				StripComponents: 0,
			})
		},
		CaddyAccessLog: *caddyLog,
	})

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := srv.Start(ctx); err != nil {
		logger.Fatalf("start: %v", err)
	}

	<-ctx.Done()
	logger.Println("shutting down")

	if err := srv.Stop(10 * time.Second); err != nil {
		logger.Printf("stop: %v", err)
	}
}

// envOr returns the value of env var `name` if set + non-empty,
// else `fallback`. Used to seed flag defaults so systemd
// Environment= lines override flags' built-in defaults without
// the operator having to also pass the matching flag.
func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}

	return fallback
}

// parseDurationOr seeds a duration flag from an env var. Falls
// back to `fallback` when the var is unset OR fails to parse —
// printing a parse error here would race with flag.Parse's own
// error path and confuse the user. Bad input silently degrades
// to the default; operator notices via the controller log line
// that reports the active config.
func parseDurationOr(name string, fallback time.Duration) time.Duration {
	v := os.Getenv(name)
	if v == "" {
		return fallback
	}

	d, err := time.ParseDuration(v)
	if err != nil {
		return fallback
	}

	return d
}

// scratchDir picks where a deploy buffers and unpacks, on a box.
//
// The operator's --build-root wins. Otherwise `/opt/voodu/builds` — the tree
// the platform owns. /tmp is not, under `ProtectSystem=strict`, `PrivateTmp=`
// or a read-only rootfs, and the resulting error names a filesystem policy
// rather than anything about the deploy.
func scratchDir(buildRoot string) string {
	if buildRoot != "" {
		return buildRoot
	}

	return paths.BuildsDir()
}
