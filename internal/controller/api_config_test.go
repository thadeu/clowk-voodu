package controller

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// postBody fires a POST and closes the body. Wraps the t.Fatal on
// network failure so tests stay readable; loose-error pattern is
// what the rest of the controller test suite uses.
func postBody(t *testing.T, url, body string) *http.Response {
	t.Helper()

	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}

	return resp
}

// TestConfig_PostThenGetRoundtripsKeyValues is the canonical happy
// path: POST a {KEY:VALUE} object to /config, then GET it back and
// confirm the same data lands in the response.
func TestConfig_PostThenGetRoundtripsKeyValues(t *testing.T) {
	api, _ := newTestAPI(t)
	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp := postBody(t, ts.URL+"/config?scope=clowk-lp&name=web&restart=false", `{"FOO":"bar","NODE_ENV":"production"}`)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("set status=%d", resp.StatusCode)
	}

	resp2, err := http.Get(ts.URL + "/config?scope=clowk-lp&name=web")
	if err != nil {
		t.Fatal(err)
	}

	defer resp2.Body.Close()

	var env struct {
		Status string `json:"status"`
		Data   struct {
			Vars map[string]string `json:"vars"`
		} `json:"data"`
	}

	_ = json.NewDecoder(resp2.Body).Decode(&env)

	if env.Data.Vars["FOO"] != "bar" || env.Data.Vars["NODE_ENV"] != "production" {
		t.Errorf("vars round-trip failed: %+v", env.Data.Vars)
	}
}

// TestConfig_AppOverridesScope confirms the precedence contract:
// app-level keys win over scope-level on conflict, both surfaced
// in the merged GET response.
func TestConfig_AppOverridesScope(t *testing.T) {
	api, _ := newTestAPI(t)
	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	r := postBody(t, ts.URL+"/config?scope=clowk-lp&restart=false", `{"FOO":"scope","BAR":"scopeonly"}`)
	r.Body.Close()

	r = postBody(t, ts.URL+"/config?scope=clowk-lp&name=web&restart=false", `{"FOO":"app","APP_KEY":"present"}`)
	r.Body.Close()

	resp, err := http.Get(ts.URL + "/config?scope=clowk-lp&name=web")
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()

	var env struct {
		Data struct {
			Vars map[string]string `json:"vars"`
		} `json:"data"`
	}

	_ = json.NewDecoder(resp.Body).Decode(&env)

	if env.Data.Vars["FOO"] != "app" {
		t.Errorf("app should override scope: FOO=%q want app", env.Data.Vars["FOO"])
	}

	if env.Data.Vars["BAR"] != "scopeonly" {
		t.Errorf("scope-only key missing: BAR=%q", env.Data.Vars["BAR"])
	}

	if env.Data.Vars["APP_KEY"] != "present" {
		t.Errorf("app-only key missing: APP_KEY=%q", env.Data.Vars["APP_KEY"])
	}
}

// TestConfig_GetSingleKeyReturnsScalar confirms ?key=KEY returns a
// flat {KEY:VALUE} map instead of the nested vars envelope.
func TestConfig_GetSingleKeyReturnsScalar(t *testing.T) {
	api, _ := newTestAPI(t)
	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	r := postBody(t, ts.URL+"/config?scope=clowk-lp&restart=false", `{"FOO":"bar"}`)
	r.Body.Close()

	resp, err := http.Get(ts.URL + "/config?scope=clowk-lp&key=FOO")
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()

	var env struct {
		Data map[string]string `json:"data"`
	}

	_ = json.NewDecoder(resp.Body).Decode(&env)

	if env.Data["FOO"] != "bar" {
		t.Errorf("key path: %+v", env.Data)
	}
}

// TestConfig_GetMissingKeyReturns404 keeps the typo-friendly
// behaviour: an operator who asks for a key that's not set sees a
// clear 404 rather than `KEY=`.
func TestConfig_GetMissingKeyReturns404(t *testing.T) {
	api, _ := newTestAPI(t)
	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/config?scope=clowk-lp&key=NOPE")
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status=%d want 404", resp.StatusCode)
	}
}

// TestConfig_DeleteKeyRemovesIt covers the unset path — DELETE
// strips a key, follow-up GET no longer surfaces it.
func TestConfig_DeleteKeyRemovesIt(t *testing.T) {
	api, _ := newTestAPI(t)
	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	r := postBody(t, ts.URL+"/config?scope=clowk-lp&restart=false", `{"FOO":"bar","BAZ":"qux"}`)
	r.Body.Close()

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/config?scope=clowk-lp&key=FOO&restart=false", nil)
	delResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	delResp.Body.Close()

	resp, err := http.Get(ts.URL + "/config?scope=clowk-lp")
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()

	var env struct {
		Data struct {
			Vars map[string]string `json:"vars"`
		} `json:"data"`
	}

	_ = json.NewDecoder(resp.Body).Decode(&env)

	if _, exists := env.Data.Vars["FOO"]; exists {
		t.Errorf("FOO should be deleted, got %+v", env.Data.Vars)
	}

	if env.Data.Vars["BAZ"] != "qux" {
		t.Errorf("BAZ should remain, got %+v", env.Data.Vars)
	}
}

// TestConfig_PostRejectsMissingScope is the input-validation guard:
// scope is required for every config operation.
func TestConfig_PostRejectsMissingScope(t *testing.T) {
	api, _ := newTestAPI(t)
	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/config", "application/json",
		bytes.NewReader([]byte(`{"FOO":"bar"}`)))
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status=%d want 400", resp.StatusCode)
	}
}

// TestConfig_RestartFalseSkipsReconcile confirms ?restart=false
// completes 200 even when there's no manifest in store. Locks in
// the "operation succeeds without side effects" path.
func TestConfig_RestartFalseSkipsReconcile(t *testing.T) {
	api, _ := newTestAPI(t)
	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp := postBody(t, ts.URL+"/config?scope=clowk-lp&restart=false", `{"FOO":"bar"}`)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status=%d want 200", resp.StatusCode)
	}
}

// TestConfig_FansOutRestartToStatefulset pins the F2.2 fix.
// `vd redis:failover` lands a config_set on the redis bucket; the
// fan-out must re-fire the statefulset's apply so the env-change
// rolling restart picks up the new REDIS_MASTER_ORDINAL. Before
// the fix, the kinds list was [deployment, job, cronjob] and
// statefulsets stayed wedged on the old bucket value.
//
// Test shape: pre-store a statefulset manifest, POST /config to
// set a value, then read the manifest back and confirm its
// metadata.revision bumped (memStore.Put increments revision on
// every successful write). A revision bump proves Put was called
// — i.e. the fan-out reached statefulsets.
func TestConfig_FansOutRestartToStatefulset(t *testing.T) {
	api, store := newTestAPI(t)
	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	// Pre-store a statefulset manifest. Set initial revision is
	// what the post-config-set Put will bump.
	body := `{"kind":"statefulset","scope":"clowk-lp","name":"redis","spec":{"image":"redis:7","replicas":3}}`

	resp := postBody(t, ts.URL+"/apply", body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("apply status=%d", resp.StatusCode)
	}

	pre, err := store.Get(t.Context(), KindStatefulset, "clowk-lp", "redis")
	if err != nil || pre == nil {
		t.Fatalf("manifest missing post-apply: %v", err)
	}

	preRevision := pre.Metadata.Revision

	// POST a config_set on the same (scope, name). Without the
	// fan-out fix, this would NOT re-Put the statefulset (kinds
	// list excluded statefulset), so the manifest revision stays
	// where it was.
	resp = postBody(t, ts.URL+"/config?scope=clowk-lp&name=redis", `{"REDIS_MASTER_ORDINAL":"1"}`)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("config status=%d", resp.StatusCode)
	}

	post, err := store.Get(t.Context(), KindStatefulset, "clowk-lp", "redis")
	if err != nil || post == nil {
		t.Fatalf("manifest missing post-config: %v", err)
	}

	if post.Metadata.Revision <= preRevision {
		t.Errorf("statefulset revision didn't bump after config_set (%d → %d) — fan-out missing statefulset?",
			preRevision, post.Metadata.Revision)
	}
}

// TestConfig_PostMaterializesEnvFile pins the env-from gap fix:
// `vd config <scope>/<name> set K=V` MUST write the .env file on disk
// at /opt/voodu/apps/<scope>-<name>/shared/.env, NOT only to etcd.
//
// Before the fix, a virtual bucket created via `vd config set`
// without a companion deployment/job/statefulset of the same
// (scope, name) lived ONLY in etcd. The env_from resolver's
// os.Stat() on the canonical path would then fail and the
// reconciler logged "no env files resolved" — silently breaking
// any deployment that env_from'd the bucket.
//
// The materialization is best-effort: failures log but don't fail
// the request. This test asserts the happy path lands the file
// at the right path with the right content.
func TestConfig_PostMaterializesEnvFile(t *testing.T) {
	// Redirect /opt/voodu to a tmpdir so the test doesn't touch
	// the real install. paths.Root() honours VOODU_ROOT.
	tmp := t.TempDir()
	t.Setenv("VOODU_ROOT", tmp)

	api, _ := newTestAPI(t)
	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp := postBody(t, ts.URL+"/config?scope=fsw&name=shared&restart=false",
		`{"DATABASE_URL":"postgres://x","REDIS_URL":"redis://y"}`)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("config POST status=%d", resp.StatusCode)
	}

	envPath := tmp + "/apps/fsw-shared/shared/.env"
	body, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("env file not materialized at %s: %v", envPath, err)
	}

	got := string(body)
	for _, want := range []string{"DATABASE_URL=postgres://x", "REDIS_URL=redis://y"} {
		if !strings.Contains(got, want) {
			t.Errorf("env file missing %q, got:\n%s", want, got)
		}
	}
}

// TestConfig_DeleteUpdatesEnvFile pins that deleting a key from a
// bucket re-materializes the .env without that key. Without this,
// containers reading the .env (via env_from) would keep seeing
// stale values for keys the operator just deleted.
func TestConfig_DeleteUpdatesEnvFile(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("VOODU_ROOT", tmp)

	api, _ := newTestAPI(t)
	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp := postBody(t, ts.URL+"/config?scope=fsw&name=shared&restart=false",
		`{"KEEP":"yes","DROPME":"obsolete"}`)
	resp.Body.Close()

	envPath := tmp + "/apps/fsw-shared/shared/.env"

	// Sanity: both keys land initially.
	body, _ := os.ReadFile(envPath)
	if !strings.Contains(string(body), "DROPME=obsolete") {
		t.Fatalf("setup: DROPME missing from initial materialization:\n%s", body)
	}

	// Delete DROPME.
	req, _ := http.NewRequest(http.MethodDelete,
		ts.URL+"/config?scope=fsw&name=shared&key=DROPME&restart=false", nil)

	delResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	delResp.Body.Close()

	if delResp.StatusCode != http.StatusOK {
		t.Fatalf("delete status=%d", delResp.StatusCode)
	}

	body, err = os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("env file gone after delete: %v", err)
	}

	got := string(body)

	if strings.Contains(got, "DROPME") {
		t.Errorf("DROPME still present after delete, env file:\n%s", got)
	}

	if !strings.Contains(got, "KEEP=yes") {
		t.Errorf("KEEP got nuked by delete (should only have removed DROPME):\n%s", got)
	}
}

// TestConfig_PostScopeLevelDoesNotMaterializeEnvFile pins the
// "name is required for materialization" contract. Scope-level
// configs (name=="") are merge-bases used at apply time by
// per-resource handlers — they don't map to a single .env file.
// The handler must skip materialization in that case rather than
// writing to a malformed path.
func TestConfig_PostScopeLevelDoesNotMaterializeEnvFile(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("VOODU_ROOT", tmp)

	api, _ := newTestAPI(t)
	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp := postBody(t, ts.URL+"/config?scope=fsw&restart=false", `{"FOO":"bar"}`)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("scope-level config status=%d", resp.StatusCode)
	}

	// No .env file should exist under /apps for a bare scope —
	// scope-level configs are merge-bases, not standalone buckets.
	matches, _ := filepath.Glob(tmp + "/apps/*/shared/.env")
	if len(matches) != 0 {
		t.Errorf("scope-level config materialized unexpected env files: %v", matches)
	}
}

// ── ?values=false ──────────────────────────────────────────────────────────

// The redaction is a MODE OF THE ENDPOINT. A console that fetched values and
// declined to draw them would have already carried them across the internet
// and into a browser's network panel. Answered here, there is nothing to
// expose — which is the whole reason this is not a UI concern.
func TestConfig_ValuesFalseNeverSendsAValue(t *testing.T) {
	api, _ := newTestAPI(t)
	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp := postBody(t, ts.URL+"/config?scope=acme&name=api&restart=false",
		`{"DATABASE_URL":"postgres://user:hunter2@db/prod","NODE_ENV":"production"}`)
	resp.Body.Close()

	body := getBody(t, ts.URL+"/config?scope=acme&name=api&values=false")

	for _, secret := range []string{"hunter2", "postgres://", "production"} {
		if strings.Contains(body, secret) {
			t.Errorf("a value reached the response: %q is in %s", secret, body)
		}
	}

	// The KEYS are the point of the read — an operator has to see what exists
	// before they can decide to reveal one.
	for _, key := range []string{"DATABASE_URL", "NODE_ENV"} {
		if !strings.Contains(body, key) {
			t.Errorf("key %q missing from %s", key, body)
		}
	}
}

// A DIFFERENT SHAPE, not the same shape with the values blanked. A caller that
// read a redacted response and wrote it back — the obvious edit-then-save round
// trip — would set every variable to the empty string and wipe the bucket.
// `vars` being absent is what makes that code impossible to write by accident.
func TestConfig_ValuesFalseOmitsVarsEntirely(t *testing.T) {
	api, _ := newTestAPI(t)
	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp := postBody(t, ts.URL+"/config?scope=acme&name=api&restart=false", `{"FOO":"bar"}`)
	resp.Body.Close()

	var env struct {
		Data struct {
			Vars     map[string]string `json:"vars"`
			Redacted bool              `json:"redacted"`
			Keys     []struct {
				Key         string `json:"key"`
				ValueDigest string `json:"value_digest"`
			} `json:"keys"`
		} `json:"data"`
	}

	decodeJSON(t, ts.URL+"/config?scope=acme&name=api&values=false", &env)

	if env.Data.Vars != nil {
		t.Errorf("vars must be absent on a redacted read, got %+v", env.Data.Vars)
	}

	if !env.Data.Redacted {
		t.Error("the response must say it is redacted")
	}

	if len(env.Data.Keys) != 1 || env.Data.Keys[0].Key != "FOO" {
		t.Fatalf("keys = %+v", env.Data.Keys)
	}

	if env.Data.Keys[0].ValueDigest == "" {
		t.Error("digest missing")
	}
}

// The digest answers the only question a value would that the key does not:
// did staging and production drift, or is it the same string in both.
func TestConfig_ValuesFalseDigestTracksTheValue(t *testing.T) {
	api, _ := newTestAPI(t)
	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	digest := func() string {
		var env struct {
			Data struct {
				Keys []struct {
					Key         string `json:"key"`
					ValueDigest string `json:"value_digest"`
				} `json:"keys"`
			} `json:"data"`
		}

		decodeJSON(t, ts.URL+"/config?scope=acme&name=api&values=false", &env)

		if len(env.Data.Keys) != 1 {
			t.Fatalf("keys = %+v", env.Data.Keys)
		}

		return env.Data.Keys[0].ValueDigest
	}

	resp := postBody(t, ts.URL+"/config?scope=acme&name=api&restart=false", `{"FOO":"one"}`)
	resp.Body.Close()

	first := digest()

	resp = postBody(t, ts.URL+"/config?scope=acme&name=api&restart=false", `{"FOO":"one"}`)
	resp.Body.Close()

	if digest() != first {
		t.Error("re-setting the same value changed the digest")
	}

	resp = postBody(t, ts.URL+"/config?scope=acme&name=api&restart=false", `{"FOO":"two"}`)
	resp.Body.Close()

	if digest() == first {
		t.Error("a changed value kept the same digest")
	}
}

// `?key=X&values=false` is a strange request, and answering it with the value
// would make values=false a suggestion.
func TestConfig_ValuesFalseAlsoRedactsASingleKeyRead(t *testing.T) {
	api, _ := newTestAPI(t)
	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp := postBody(t, ts.URL+"/config?scope=acme&name=api&restart=false", `{"TOKEN":"hunter2"}`)
	resp.Body.Close()

	body := getBody(t, ts.URL+"/config?scope=acme&name=api&key=TOKEN&values=false")

	if strings.Contains(body, "hunter2") {
		t.Errorf("single-key read leaked the value: %s", body)
	}

	if !strings.Contains(body, "TOKEN") {
		t.Errorf("single-key read lost the key: %s", body)
	}
}

// Sorted, because a map iterates at random and a screen whose rows reshuffle
// on every refresh is a screen nobody can read down.
func TestConfig_ValuesFalseSortsKeys(t *testing.T) {
	api, _ := newTestAPI(t)
	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp := postBody(t, ts.URL+"/config?scope=acme&name=api&restart=false",
		`{"ZED":"1","ALPHA":"2","MIKE":"3"}`)
	resp.Body.Close()

	var env struct {
		Data struct {
			Keys []struct {
				Key string `json:"key"`
			} `json:"keys"`
		} `json:"data"`
	}

	decodeJSON(t, ts.URL+"/config?scope=acme&name=api&values=false", &env)

	got := make([]string, 0, len(env.Data.Keys))
	for _, k := range env.Data.Keys {
		got = append(got, k.Key)
	}

	want := []string{"ALPHA", "MIKE", "ZED"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("keys = %v, want %v", got, want)
	}
}

// Omitting the parameter must keep the old shape exactly. Redaction that
// leaked into the default read would break every existing caller.
func TestConfig_WithoutValuesFalseTheShapeIsUnchanged(t *testing.T) {
	api, _ := newTestAPI(t)
	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp := postBody(t, ts.URL+"/config?scope=acme&name=api&restart=false", `{"FOO":"bar"}`)
	resp.Body.Close()

	var env struct {
		Data struct {
			Vars     map[string]string `json:"vars"`
			Redacted bool              `json:"redacted"`
		} `json:"data"`
	}

	decodeJSON(t, ts.URL+"/config?scope=acme&name=api", &env)

	if env.Data.Vars["FOO"] != "bar" {
		t.Errorf("default read lost its values: %+v", env.Data.Vars)
	}

	if env.Data.Redacted {
		t.Error("a default read must not claim to be redacted")
	}
}

// getBody reads a GET into a string, for the tests that assert on what is NOT
// in the response — a decode would only see the fields it declares.
func getBody(t *testing.T, url string) string {
	t.Helper()

	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()

	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		t.Fatal(err)
	}

	return buf.String()
}

func decodeJSON(t *testing.T, url string, into any) {
	t.Helper()

	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()

	if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
		t.Fatal(err)
	}
}

// ── the PAT plane ──────────────────────────────────────────────────────────

// THE acceptance criterion of the config routes: reading which variables exist
// and setting one are DIFFERENT powers, and the scopes say so.
//
// The split matters because of who holds each token. A console that only ever
// shows config should not carry a token that can rewrite production env, and a
// token that can rewrite it should not be the one handed to a dashboard.
//
// SCOPES MATCH EXACTLY. `config` does NOT imply `config:read`, the same way
// `actions` does not imply `read` — scopeOrder is display ordering, not a
// hierarchy. A PAT that both reads and writes config carries both scopes.
//
// Pinned because the opposite is the intuitive guess, and a reader who assumes
// a hierarchy would grant `config` alone and get a 403 on the first page load.
// If that ever becomes a hierarchy it should be one model-wide decision, not a
// special case for config.
func TestConfigRoutesRequireTheirScopes(t *testing.T) {
	store := newMemStore()
	auth := newPATAuthorizer(store, quietLogger())

	tokens := map[Scope]string{}

	for _, s := range []Scope{ScopeRead, ScopeConfigRead, ScopeConfig, ScopeActions, ScopeDeploy} {
		plain, _ := seedTestPAT(t, store, []Scope{s})
		tokens[s] = plain
	}

	routes := []struct {
		method   string
		path     string
		required Scope
		// allowed is every scope that must reach this route.
		allowed []Scope
	}{
		{http.MethodGet, "/api/pat/v1/config", ScopeConfigRead, []Scope{ScopeConfigRead}},
		{http.MethodPost, "/api/pat/v1/config", ScopeConfig, []Scope{ScopeConfig}},
		{http.MethodDelete, "/api/pat/v1/config", ScopeConfig, []Scope{ScopeConfig}},
	}

	for _, route := range routes {
		for scope, plain := range tokens {
			name := route.method + " " + route.path + " with " + string(scope)

			t.Run(name, func(t *testing.T) {
				req := httptest.NewRequest(route.method, route.path, nil)
				signRequest(req, plain)

				rr := httptest.NewRecorder()
				called := false

				auth.Middleware(route.required, nextOK(t, &called))(rr, req)

				permitted := false

				for _, s := range route.allowed {
					if s == scope {
						permitted = true

						break
					}
				}

				if permitted {
					if rr.Code != http.StatusOK {
						t.Errorf("scope %q refused: %d (%s)", scope, rr.Code, rr.Body.String())
					}

					return
				}

				if rr.Code != http.StatusForbidden {
					t.Errorf("scope %q reached a config route: %d", scope, rr.Code)
				}

				if called {
					t.Errorf("scope %q was let through to the handler", scope)
				}
			})
		}
	}
}

// Verbatim passthrough, not a second implementation. Config behaviour has to
// be ONE behaviour — merge semantics, the restart on write, the scope/name
// resolution — and a parallel path is where the two quietly start to differ.
//
// Asserted as the same BYTES for the same input, because "looks equivalent" is
// how a divergence survives review.
func TestConfigPATPlaneAnswersByteForByteLikeTheInternalPlane(t *testing.T) {
	api, _ := newTestAPI(t)
	ts := httptest.NewServer(api.Handler())

	defer ts.Close()

	resp := postBody(t, ts.URL+"/config?scope=acme&name=api&restart=false",
		`{"FOO":"bar","BAZ":"qux"}`)
	resp.Body.Close()

	for _, query := range []string{
		"scope=acme&name=api",
		"scope=acme&name=api&values=false",
		"scope=acme&name=api&key=FOO",
		"scope=acme",
	} {
		internal := recordConfigGet(t, api, "/config?"+query)
		plane := recordConfigGet(t, api, "/api/pat/v1/config?"+query)

		if internal != plane {
			t.Errorf("planes disagree on %q:\n internal: %s\n    plane: %s", query, internal, plane)
		}
	}
}

// recordConfigGet calls the handler directly rather than over the PAT plane's
// signature check: what is under test is the RESPONSE, and threading a signed
// request through would be testing the authorizer a second time.
func recordConfigGet(t *testing.T, api *API, path string) string {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, path, nil)
	rr := httptest.NewRecorder()

	if strings.HasPrefix(path, "/api/pat/v1/") {
		api.handlePATConfigGet(rr, req)
	} else {
		api.configGet(rr, req)
	}

	return rr.Body.String()
}
