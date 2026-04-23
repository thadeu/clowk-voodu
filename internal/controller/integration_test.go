package controller

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// freeTCPPort returns a locally bound TCP port and releases it. The port
// might be reclaimed before the caller can rebind, but for a single-node
// loopback test this is good enough.
func freeTCPPort(t *testing.T) int {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	defer l.Close()

	return l.Addr().(*net.TCPAddr).Port
}

func startServer(t *testing.T, dataDir string) *Server {
	t.Helper()

	clientPort := freeTCPPort(t)
	peerPort := freeTCPPort(t)

	srv := NewServer(Config{
		DataDir:    dataDir,
		HTTPAddr:   "127.0.0.1:0",
		EtcdClient: fmt.Sprintf("http://127.0.0.1:%d", clientPort),
		EtcdPeer:   fmt.Sprintf("http://127.0.0.1:%d", peerPort),
		NodeName:   "voodu-test",
		Version:    "test",
		Logger:     log.New(io.Discard, "", 0),
		QuietEtcd:  true,
	})

	if err := srv.Start(t.Context()); err != nil {
		t.Fatalf("start server: %v", err)
	}

	return srv
}

// TestControllerPersistsAcrossRestart is the explicit M3 done criterion:
// POST /apply → stop controller → restart on same DataDir → GET /apply
// returns the same manifest.
func TestControllerPersistsAcrossRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping embedded-etcd integration test in -short mode")
	}

	dataDir := t.TempDir()

	srv := startServer(t, dataDir)

	body := `{"kind":"deployment","scope":"test","name":"api","spec":{"image":"nginx:1"}}`

	resp, err := http.Post(
		"http://"+srv.HTTPAddr()+"/apply",
		"application/json",
		strings.NewReader(body),
	)
	if err != nil {
		t.Fatalf("post apply: %v", err)
	}

	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("apply returned %d", resp.StatusCode)
	}

	if err := srv.Stop(5 * time.Second); err != nil {
		t.Fatalf("stop: %v", err)
	}

	// Give etcd a beat to release filesystem locks before we re-open it.
	time.Sleep(200 * time.Millisecond)

	srv2 := startServer(t, dataDir)

	defer srv2.Stop(5 * time.Second)

	resp2, err := http.Get("http://" + srv2.HTTPAddr() + "/apply")
	if err != nil {
		t.Fatalf("get apply: %v", err)
	}

	defer resp2.Body.Close()

	var env struct {
		Status string
		Data   []Manifest
	}

	if err := json.NewDecoder(resp2.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if env.Status != "ok" || len(env.Data) != 1 {
		t.Fatalf("state not persisted: %+v", env)
	}

	got := env.Data[0]

	if got.Kind != KindDeployment || got.Name != "api" {
		t.Errorf("wrong manifest after restart: %+v", got)
	}

	if !bytes.Contains(got.Spec, []byte("nginx:1")) {
		t.Errorf("spec not preserved: %s", got.Spec)
	}
}
