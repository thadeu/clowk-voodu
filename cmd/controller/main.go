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
	var (
		httpAddr    = flag.String("http", ":8686", "HTTP API listen address")
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
		DataDir:     *dataDir,
		HTTPAddr:    *httpAddr,
		EtcdClient:  *etcdClient,
		EtcdPeer:    *etcdPeer,
		NodeName:    *nodeName,
		PluginsRoot: *pluginsDir,
		Version:     fmt.Sprintf("%s (commit: %s)", version, commit),
		Logger:      logger,
		QuietEtcd:   *quietEtcd,
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
