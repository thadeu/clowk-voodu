package controller

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHandlePATCreate_HappyPath pins the create → list → use lifecycle
// at the HTTP layer: the plain token shows up in the create response,
// the redacted record makes it into list, and the hash NEVER appears
// in any response body (regression assertion for the "shown once"
// contract).
func TestHandlePATCreate_HappyPath(t *testing.T) {
	api, _ := newTestAPI(t)
	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	body := `{"scopes":["read","actions"],"name":"webui-staging"}`
	resp, err := http.Post(ts.URL+"/pats", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status: got %d, want 201", resp.StatusCode)
	}

	var env struct {
		Status string `json:"status"`
		Data   struct {
			Token  string `json:"token"`
			Record struct {
				ID     string   `json:"id"`
				Scopes []string `json:"scopes"`
				Name   string   `json:"name"`
			} `json:"record"`
		} `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatal(err)
	}

	if env.Status != "ok" {
		t.Errorf("status: %q, want ok", env.Status)
	}

	if !strings.HasPrefix(env.Data.Token, "pat_") {
		t.Errorf("token missing pat_ prefix: %q", env.Data.Token)
	}

	// pat_ (4) + 6-char ID + 22-char secret = 32.
	if len(env.Data.Token) != 32 {
		t.Errorf("token length: %d, want 32", len(env.Data.Token))
	}

	if env.Data.Record.Name != "webui-staging" {
		t.Errorf("name: %q, want webui-staging", env.Data.Record.Name)
	}

	if len(env.Data.Record.Scopes) != 2 {
		t.Errorf("scopes: %v, want [read actions]", env.Data.Record.Scopes)
	}
}

// TestHandlePATCreate_RejectsEmptyScopes pins the validation gate.
// A PAT with no scopes is useless (every endpoint requires SOME
// scope); minting one would only confuse the operator.
func TestHandlePATCreate_RejectsEmptyScopes(t *testing.T) {
	api, _ := newTestAPI(t)
	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	body := `{"scopes":[],"name":"empty"}`

	resp, err := http.Post(ts.URL+"/pats", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", resp.StatusCode)
	}
}

// TestHandlePATCreate_RejectsUnknownScope mirrors the parse-time
// rejection at the HTTP layer.
func TestHandlePATCreate_RejectsUnknownScope(t *testing.T) {
	api, _ := newTestAPI(t)
	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	body := `{"scopes":["admin"]}`

	resp, err := http.Post(ts.URL+"/pats", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", resp.StatusCode)
	}
}

// TestHandlePATList_RedactsHash is the critical secret-hygiene
// regression test. The list endpoint MUST NEVER return HashHex
// — a malicious operator with curl access could otherwise
// extract every stored hash for offline brute force.
func TestHandlePATList_RedactsHash(t *testing.T) {
	api, store := newTestAPI(t)

	_, rec, _ := GeneratePAT([]Scope{ScopeRead}, "hidden")
	rec.HashHex = "ABABABAB-this-must-never-leak"
	_ = store.PutPAT(t.Context(), rec)

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/pats")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body := readAll(t, resp.Body)

	if strings.Contains(body, "ABABABAB-this-must-never-leak") {
		t.Errorf("HashHex leaked in /pats response:\n%s", body)
	}

	if strings.Contains(body, "hash_hex") {
		t.Errorf("hash_hex field present in /pats response (must be redacted):\n%s", body)
	}
}

// TestHandlePATRevoke_LifecyclePin covers the full lifecycle through
// HTTP: create → list (sees it) → revoke → list (gone).
func TestHandlePATRevoke_LifecyclePin(t *testing.T) {
	api, _ := newTestAPI(t)
	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	// Create.
	create := func() string {
		body := `{"scopes":["read"]}`
		resp, err := http.Post(ts.URL+"/pats", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		var env struct {
			Data struct {
				Record struct {
					ID string `json:"id"`
				} `json:"record"`
			} `json:"data"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&env)

		return env.Data.Record.ID
	}

	id := create()
	if id == "" {
		t.Fatal("create returned empty ID")
	}

	// List sees it.
	{
		resp, err := http.Get(ts.URL + "/pats")
		if err != nil {
			t.Fatal(err)
		}
		body := readAll(t, resp.Body)

		if !strings.Contains(body, id) {
			t.Errorf("list missing freshly-created PAT id %s:\n%s", id, body)
		}
	}

	// Revoke.
	{
		req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/pats/"+id, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("revoke status: got %d, want 200", resp.StatusCode)
		}
	}

	// List does NOT see it.
	{
		resp, err := http.Get(ts.URL + "/pats")
		if err != nil {
			t.Fatal(err)
		}
		body := readAll(t, resp.Body)

		if strings.Contains(body, id) {
			t.Errorf("list still contains revoked PAT id %s:\n%s", id, body)
		}
	}

	// Revoke again → 404.
	{
		req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/pats/"+id, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("revoke-twice status: got %d, want 404", resp.StatusCode)
		}
	}
}

// TestHandlePATList_SortedByCreatedAtDesc pins the newest-first
// ordering. `vd pat list` consumers (operators scanning for the
// PAT they just created) rely on this.
func TestHandlePATList_SortedByCreatedAtDesc(t *testing.T) {
	api, _ := newTestAPI(t)
	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	mk := func(name string) {
		body := `{"scopes":["read"],"name":"` + name + `"}`
		resp, err := http.Post(ts.URL+"/pats", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}

	mk("first")
	mk("second")
	mk("third")

	resp, err := http.Get(ts.URL + "/pats")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var env struct {
		Data struct {
			PATs []struct {
				Name string `json:"name"`
			} `json:"pats"`
		} `json:"data"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&env)

	if len(env.Data.PATs) != 3 {
		t.Fatalf("expected 3 PATs, got %d", len(env.Data.PATs))
	}

	if env.Data.PATs[0].Name != "third" {
		t.Errorf("order[0]: %q, want third (newest first)", env.Data.PATs[0].Name)
	}

	if env.Data.PATs[2].Name != "first" {
		t.Errorf("order[2]: %q, want first (oldest last)", env.Data.PATs[2].Name)
	}
}

// TestHandlePATCreate_TokenNeverInResponseAfterCreate pins that
// the plain token appears EXACTLY ONCE — only in the create
// response. Subsequent calls (list, anything) never see it.
func TestHandlePATCreate_TokenNeverInResponseAfterCreate(t *testing.T) {
	api, _ := newTestAPI(t)
	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	body := `{"scopes":["read"]}`
	resp, err := http.Post(ts.URL+"/pats", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var env struct {
		Data struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&env)

	plain := env.Data.Token
	if plain == "" {
		t.Fatal("no token in create response")
	}

	// List endpoint must not contain the plain.
	lr, err := http.Get(ts.URL + "/pats")
	if err != nil {
		t.Fatal(err)
	}
	defer lr.Body.Close()

	listBody := readAll(t, lr.Body)
	if strings.Contains(listBody, plain) {
		t.Errorf("plain token leaked into /pats response:\n%s", listBody)
	}
}

// readAll is a tiny helper that drains a response body to string.
// The existing newTestAPI helper in api_test.go gives us the API +
// memstore; this helper keeps the PAT tests independent.
func readAll(t *testing.T, body interface {
	Read(p []byte) (n int, err error)
}) string {
	t.Helper()

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(body)

	return buf.String()
}
