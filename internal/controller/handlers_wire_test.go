package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"

	"go.voodu.clowk.in/internal/activity"
)

func newWireAPI(t *testing.T) (*API, *fakeWG, string) {
	t.Helper()

	api, store, dir := newActivityAPI(t)

	wg := &fakeWG{}

	api.Wire = &Wire{
		Store:        store,
		ConfPath:     filepath.Join(t.TempDir(), "peers.conf"),
		Run:          wg.run,
		RunIP:        (&fakeIP{}).run,
		LocalAddress: func() netip.Addr { return netip.MustParseAddr("10.254.91.221") },
		OutboundIP:   func() netip.Addr { return netip.MustParseAddr("152.53.91.221") },
	}

	return api, wg, dir
}

func doJSON(t *testing.T, method, url, body string) (int, map[string]any) {
	t.Helper()

	req, _ := http.NewRequest(method, url, strings.NewReader(body))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()

	var env map[string]any

	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatal(err)
	}

	return resp.StatusCode, env
}

func TestHandleWire_AddListRemove(t *testing.T) {
	api, wg, dir := newWireAPI(t)
	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	code, env := doJSON(t, http.MethodPost, ts.URL+"/wire/peers", `{"public_key":"`+testKey(2)+`","address":"10.254.167.105","endpoint":"152.53.167.105:51820"}`)
	if code != http.StatusOK {
		t.Fatalf("add: %d %v", code, env)
	}

	if wg.applies() != 1 {
		t.Fatalf("add did not apply to wg0: %v", wg.calls)
	}

	wg.dump = "privkey\t" + testKey(1) + "\t51820\toff\n" +
		testKey(2) + "\t(none)\t152.53.167.105:51820\t10.254.167.105/32\t1757700000\t10\t20\t25\n"

	code, env = doJSON(t, http.MethodGet, ts.URL+"/wire", "")
	if code != http.StatusOK {
		t.Fatalf("status: %d %v", code, env)
	}

	data := env["data"].(map[string]any)

	if id := data["identity"].(map[string]any); id["address"] != "10.254.91.221" || id["endpoint"] != "152.53.91.221:51820" {
		t.Fatalf("identity = %v", id)
	}

	peers := data["peers"].([]any)
	if len(peers) != 1 {
		t.Fatalf("peers = %v", peers)
	}

	if p := peers[0].(map[string]any); p["applied"] != true || p["link"] == nil {
		t.Fatalf("peer = %v, want applied with link", p)
	}

	code, env = doJSON(t, http.MethodDelete, ts.URL+"/wire/peers/10.254.167.105", "")
	if code != http.StatusOK {
		t.Fatalf("remove: %d %v", code, env)
	}

	code, _ = doJSON(t, http.MethodDelete, ts.URL+"/wire/peers/10.254.167.105", "")
	if code != http.StatusNotFound {
		t.Fatalf("remove again: %d, want 404", code)
	}

	var actions []activity.Action

	for _, rec := range trail(t, dir) {
		actions = append(actions, rec.Action)
	}

	want := []activity.Action{activity.ActionWireAdd, activity.ActionWireRemove, activity.ActionWireRemove}

	if len(actions) != len(want) {
		t.Fatalf("trail = %v, want %v", actions, want)
	}

	for i := range want {
		if actions[i] != want[i] {
			t.Fatalf("trail = %v, want %v", actions, want)
		}
	}
}

func TestHandleWire_RejectsBadPeer(t *testing.T) {
	api, wg, _ := newWireAPI(t)
	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	code, env := doJSON(t, http.MethodPost, ts.URL+"/wire/peers", `{"public_key":"`+testKey(2)+`","address":"10.8.0.1"}`)
	if code != http.StatusBadRequest || !strings.Contains(env["error"].(string), "not in the voodu tunnel") {
		t.Fatalf("add: %d %v", code, env)
	}

	if wg.applies() != 0 {
		t.Fatal("a rejected peer must not touch wg0")
	}
}

func TestHandleWire_WithoutWire(t *testing.T) {
	api, _ := newTestAPI(t)
	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	code, _ := doJSON(t, http.MethodGet, ts.URL+"/wire", "")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", code)
	}
}

func TestHandleWire_WGDownIs503(t *testing.T) {
	api, wg, _ := newWireAPI(t)
	wg.fail = true
	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	code, env := doJSON(t, http.MethodPost, ts.URL+"/wire/peers", `{"public_key":"`+testKey(2)+`","address":"10.254.167.105"}`)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("add with wg down: %d %v, want 503", code, env)
	}
}

func TestHandleWire_UFW(t *testing.T) {
	api, _, _ := newWireAPI(t)
	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	code, _ := doJSON(t, http.MethodGet, ts.URL+"/wire/ufw", "")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("local voodu0: %d, want 503", code)
	}

	api.Wire.Bridge = func() string { return "br-21f70aa6d28e" }

	code, env := doJSON(t, http.MethodGet, ts.URL+"/wire/ufw", "")
	if code != http.StatusOK {
		t.Fatalf("routed: %d %v", code, env)
	}

	data := env["data"].(map[string]any)
	if data["bridge"] != "br-21f70aa6d28e" || len(data["rules"].([]any)) != 4 {
		t.Fatalf("data = %v", data)
	}
}
