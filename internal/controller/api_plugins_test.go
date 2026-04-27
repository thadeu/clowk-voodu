package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Plugin endpoints test the full HTTP surface end-to-end: install a
// local plugin, list it, exec a command, uninstall it. Anything less
// and a broken wiring (e.g. bad route pattern) wouldn't be caught until
// an operator tripped over it.
func TestPluginLifecycleEndToEnd(t *testing.T) {
	root := t.TempDir()

	api := &API{
		Store:       newMemStore(),
		Version:     "test",
		PluginsRoot: root,
		NodeName:    "voodu-test",
		EtcdClient:  "http://127.0.0.1:0",
	}

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	src := makeTestPluginSource(t, "hello", map[string]string{
		"say": "#!/bin/bash\necho \"hi $1 from $VOODU_NODE\"\n",
		"json": "#!/bin/bash\n" +
			"echo '{\"status\":\"ok\",\"data\":{\"hello\":\"'\"$1\"'\"}}'\n",
	})

	// install
	resp, err := http.Post(ts.URL+"/plugins/install", "application/json",
		strings.NewReader(`{"source":"`+src+`"}`))
	if err != nil {
		t.Fatal(err)
	}

	if resp.StatusCode != http.StatusOK {
		raw, _ := readAllClose(resp)
		t.Fatalf("install status=%d body=%s", resp.StatusCode, raw)
	}
	resp.Body.Close()

	if _, err := os.Stat(filepath.Join(root, "hello", "commands", "say")); err != nil {
		t.Fatalf("plugin not installed: %v", err)
	}

	// list
	resp, err = http.Get(ts.URL + "/plugins")
	if err != nil {
		t.Fatal(err)
	}

	var listEnv struct {
		Data struct {
			Plugins []map[string]any `json:"plugins"`
		} `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&listEnv); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if len(listEnv.Data.Plugins) != 1 || listEnv.Data.Plugins[0]["name"] != "hello" {
		t.Fatalf("list: %+v", listEnv.Data.Plugins)
	}

	// exec plain text
	resp, err = http.Post(ts.URL+"/plugins/exec", "application/json",
		strings.NewReader(`{"args":["hello","say","world"]}`))
	if err != nil {
		t.Fatal(err)
	}

	raw, _ := readAllClose(resp)

	if !strings.Contains(string(raw), "hi world") || !strings.Contains(string(raw), "voodu-test") {
		t.Errorf("plain-text exec: %s", raw)
	}

	// exec JSON envelope
	resp, err = http.Post(ts.URL+"/plugins/exec", "application/json",
		strings.NewReader(`{"args":["hello","json","earth"]}`))
	if err != nil {
		t.Fatal(err)
	}

	raw, _ = readAllClose(resp)

	var env struct {
		Status string `json:"status"`
		Data   struct {
			Hello string `json:"hello"`
		} `json:"data"`
	}

	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("envelope decode: %v (raw=%s)", err, raw)
	}

	if env.Status != "ok" || env.Data.Hello != "earth" {
		t.Errorf("envelope: %+v", env)
	}

	// remove
	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/plugins/hello", nil)

	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("remove status=%d", resp.StatusCode)
	}

	if _, err := os.Stat(filepath.Join(root, "hello")); !os.IsNotExist(err) {
		t.Errorf("plugin dir still present: %v", err)
	}

	// remove twice → 404
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("second remove: want 404, got %d", resp.StatusCode)
	}
}

func makeTestPluginSource(t *testing.T, name string, cmds map[string]string) string {
	t.Helper()

	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, "plugin.yml"),
		[]byte("name: "+name+"\nversion: 0.0.1\n"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(filepath.Join(dir, "commands"), 0755); err != nil {
		t.Fatal(err)
	}

	for cmd, body := range cmds {
		if err := os.WriteFile(filepath.Join(dir, "commands", cmd), []byte(body), 0755); err != nil {
			t.Fatal(err)
		}
	}

	return dir
}

func readAllClose(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()

	var buf [8192]byte

	n, err := resp.Body.Read(buf[:])
	if err != nil && err.Error() != "EOF" {
		return buf[:n], err
	}

	return buf[:n], nil
}
