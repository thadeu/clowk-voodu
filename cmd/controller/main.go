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

	"go.voodu.clowk.in/internal/controller"
	"go.voodu.clowk.in/internal/paths"
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
		nodeName    = flag.String("name", "voodu-0", "etcd cluster member name")
		quietEtcd   = flag.Bool("quiet-etcd", true, "suppress etcd info logging")
		showVersion = flag.Bool("version", false, "print version and exit")
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

	srv := controller.NewServer(controller.Config{
		DataDir:        *dataDir,
		HTTPAddr:       *httpAddr,
		PATAddr:        *patAddr,
		PATActionRate:  *patRate,
		PATActionBurst: *patBurst,
		EtcdClient:     *etcdClient,
		EtcdPeer:       *etcdPeer,
		NodeName:       *nodeName,
		PluginsRoot:    *pluginsDir,
		Version:        fmt.Sprintf("%s (commit: %s)", version, commit),
		Logger:         logger,
		QuietEtcd:      *quietEtcd,
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
