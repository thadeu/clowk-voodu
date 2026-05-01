package controller

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
