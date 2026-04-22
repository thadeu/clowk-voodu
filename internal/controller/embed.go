package controller

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/server/v3/embed"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// EtcdConfig is the minimal set of knobs we expose. Defaults are chosen
// so a single-node controller "just works" on a fresh box — data dir
// under the Voodu root, client/peer bound to loopback, single-member
// cluster.
type EtcdConfig struct {
	Name      string // cluster member name (default: voodu-0)
	DataDir   string // data directory (required)
	ClientURL string // http://127.0.0.1:2379
	PeerURL   string // http://127.0.0.1:2380
	// Quiet suppresses etcd's (very verbose) info logging. Errors always go through.
	Quiet bool
}

// EmbeddedEtcd is a running embedded etcd server + its in-process client.
type EmbeddedEtcd struct {
	server *embed.Etcd
	Client *clientv3.Client
}

// StartEmbeddedEtcd spins up an embedded etcd and returns it along with a
// ready-to-use client. Blocks until etcd reports it is ready, or fails
// fast after 30s.
func StartEmbeddedEtcd(cfg EtcdConfig) (*EmbeddedEtcd, error) {
	if cfg.DataDir == "" {
		return nil, fmt.Errorf("etcd: DataDir is required")
	}

	if cfg.Name == "" {
		cfg.Name = "voodu-0"
	}

	if cfg.ClientURL == "" {
		cfg.ClientURL = "http://127.0.0.1:2379"
	}

	if cfg.PeerURL == "" {
		cfg.PeerURL = "http://127.0.0.1:2380"
	}

	clientURL, err := url.Parse(cfg.ClientURL)
	if err != nil {
		return nil, fmt.Errorf("parse client url: %w", err)
	}

	peerURL, err := url.Parse(cfg.PeerURL)
	if err != nil {
		return nil, fmt.Errorf("parse peer url: %w", err)
	}

	if err := os.MkdirAll(cfg.DataDir, 0700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}

	ecfg := embed.NewConfig()
	ecfg.Name = cfg.Name
	ecfg.Dir = cfg.DataDir
	ecfg.ListenClientUrls = []url.URL{*clientURL}
	ecfg.AdvertiseClientUrls = []url.URL{*clientURL}
	ecfg.ListenPeerUrls = []url.URL{*peerURL}
	ecfg.AdvertisePeerUrls = []url.URL{*peerURL}
	ecfg.InitialCluster = fmt.Sprintf("%s=%s", cfg.Name, cfg.PeerURL)
	ecfg.InitialClusterToken = "voodu-etcd-cluster"
	ecfg.ClusterState = embed.ClusterStateFlagNew

	if cfg.Quiet {
		ecfg.Logger = "zap"
		ecfg.LogLevel = "error"

		logger, err := zap.NewStdLogAt(zap.NewNop(), zapcore.ErrorLevel)
		_ = logger

		if err == nil {
			ecfg.ZapLoggerBuilder = embed.NewZapLoggerBuilder(zap.NewNop())
		}
	}

	server, err := embed.StartEtcd(ecfg)
	if err != nil {
		return nil, fmt.Errorf("start etcd: %w", err)
	}

	select {
	case <-server.Server.ReadyNotify():
	case <-time.After(30 * time.Second):
		server.Close()
		return nil, fmt.Errorf("etcd took too long to become ready")
	}

	client, err := clientv3.New(clientv3.Config{
		Endpoints:   []string{cfg.ClientURL},
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		server.Close()
		return nil, fmt.Errorf("etcd client: %w", err)
	}

	return &EmbeddedEtcd{server: server, Client: client}, nil
}

// Close shuts down the client and server. Safe to call on a nil receiver.
func (e *EmbeddedEtcd) Close() {
	if e == nil {
		return
	}

	if e.Client != nil {
		_ = e.Client.Close()
	}

	if e.server != nil {
		e.server.Close()
	}
}

// Health pings etcd to confirm it is serving.
func (e *EmbeddedEtcd) Health(ctx context.Context) error {
	_, err := e.Client.Status(ctx, e.Client.Endpoints()[0])
	return err
}
