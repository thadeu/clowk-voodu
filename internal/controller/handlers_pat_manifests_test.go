package controller

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newManifestsPATServer serves the PAT plane with one PAT that holds the
// given scopes and resource scopes.
func newManifestsPATServer(t *testing.T, scopes []Scope, resourceScopes []string) (*httptest.Server, *API, string) {
	t.Helper()

	api, _, _, _ := newTestPATAPI(t)

	plain, rec, err := GeneratePAT(scopes, "cloud")
	if err != nil {
		t.Fatalf("GeneratePAT: %v", err)
	}

	rec.ResourceScopes = resourceScopes

	if err := api.Store.PutPAT(t.Context(), rec); err != nil {
		t.Fatalf("PutPAT: %v", err)
	}

	ts := httptest.NewServer(api.PATHandler(nil, 100, 100))
	t.Cleanup(ts.Close)

	return ts, api, plain
}

func patDo(t *testing.T, plain, method, url, body string) (int, string) {
	t.Helper()

	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}

	req, _ := http.NewRequest(method, url, reader)
	patSign(req, plain)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()

	out, _ := io.ReadAll(resp.Body)

	return resp.StatusCode, string(out)
}

const cloudStatefulset = `{"kind":"statefulset","scope":"cloud","name":"paris-roma","spec":{
	"image":"ghcr.io/thadeu/anway:0.5.2","replicas":1,
	"env":{"SERVER_PASSWORD":"x"},
	"ports":["127.0.0.1:7420:7420"],
	"volume_claims":[{"name":"data","mount_path":"/data"}]}}`

func TestPATManifests_ApplyInScope(t *testing.T) {
	ts, api, plain := newManifestsPATServer(t, []Scope{ScopeManifests}, []string{"cloud"})

	code, body := patDo(t, plain, http.MethodPost, ts.URL+"/api/pat/v1/apply", cloudStatefulset)

	if code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("apply: %d %s", code, body)
	}

	m, err := api.Store.Get(t.Context(), KindStatefulset, "cloud", "paris-roma")
	if err != nil || m == nil {
		t.Fatalf("statefulset not stored: %v", err)
	}
}

func TestPATManifests_ApplyOutOfScopeRefused(t *testing.T) {
	ts, api, plain := newManifestsPATServer(t, []Scope{ScopeManifests}, []string{"cloud"})

	body := strings.Replace(cloudStatefulset, `"scope":"cloud"`, `"scope":"clowk"`, 1)

	code, out := patDo(t, plain, http.MethodPost, ts.URL+"/api/pat/v1/apply", body)

	if code != http.StatusForbidden {
		t.Fatalf("apply other scope: got %d %s, want 403", code, out)
	}

	if m, _ := api.Store.Get(t.Context(), KindStatefulset, "clowk", "paris-roma"); m != nil {
		t.Fatal("refused apply still stored the manifest")
	}
}

func TestPATManifests_ApplyNeedsManifestsScope(t *testing.T) {
	ts, _, plain := newManifestsPATServer(t, []Scope{ScopeRead, ScopeActions, ScopeConfig}, nil)

	code, _ := patDo(t, plain, http.MethodPost, ts.URL+"/api/pat/v1/apply", cloudStatefulset)

	if code != http.StatusForbidden {
		t.Fatalf("apply without manifests scope: got %d, want 403", code)
	}
}

func TestPATManifests_ListNeedsScopeAndFilters(t *testing.T) {
	ts, api, plain := newManifestsPATServer(t, []Scope{ScopeManifests}, []string{"cloud"})

	for _, m := range []*Manifest{
		{Kind: KindStatefulset, Scope: "cloud", Name: "a", Spec: json.RawMessage(`{"image":"x"}`)},
		{Kind: KindDeployment, Scope: "clowk", Name: "web", Spec: json.RawMessage(`{"image":"x"}`)},
	} {
		if _, err := api.Store.Put(t.Context(), m); err != nil {
			t.Fatal(err)
		}
	}

	if code, _ := patDo(t, plain, http.MethodGet, ts.URL+"/api/pat/v1/manifests", ""); code != http.StatusBadRequest {
		t.Fatalf("list without scope: got %d, want 400", code)
	}

	if code, _ := patDo(t, plain, http.MethodGet, ts.URL+"/api/pat/v1/manifests?scope=clowk", ""); code != http.StatusForbidden {
		t.Fatalf("list other scope: got %d, want 403", code)
	}

	code, body := patDo(t, plain, http.MethodGet, ts.URL+"/api/pat/v1/manifests?scope=cloud", "")
	if code != http.StatusOK {
		t.Fatalf("list: %d %s", code, body)
	}

	if !strings.Contains(body, `"name":"a"`) || strings.Contains(body, `"name":"web"`) {
		t.Fatalf("list must hold only scope cloud: %s", body)
	}
}

func TestPATManifests_MutationsCheckScope(t *testing.T) {
	ts, _, plain := newManifestsPATServer(t, []Scope{ScopeManifests}, []string{"cloud"})

	routes := []struct{ method, path string }{
		{http.MethodDelete, "/api/pat/v1/resource?scope=clowk&name=web"},
		{http.MethodDelete, "/api/pat/v1/scope?scope=clowk"},
		{http.MethodPost, "/api/pat/v1/restart?kind=deployment&scope=clowk&name=web"},
		{http.MethodPost, "/api/pat/v1/rollback?kind=deployment&scope=clowk&name=web"},
		{http.MethodGet, "/api/pat/v1/describe?kind=deployment&scope=clowk&name=web"},
		{http.MethodPost, "/api/pat/v1/pods/clowk-web.a3f9/stop"},
	}

	for _, rt := range routes {
		if code, out := patDo(t, plain, rt.method, ts.URL+rt.path, ""); code != http.StatusForbidden {
			t.Errorf("%s %s: got %d %s, want 403", rt.method, rt.path, code, out)
		}
	}

	if code, _ := patDo(t, plain, http.MethodDelete, ts.URL+"/api/pat/v1/resource?name=web", ""); code != http.StatusBadRequest {
		t.Errorf("delete without scope: got %d, want 400", code)
	}
}

func TestPATManifests_ConfigLimitedToResourceScopes(t *testing.T) {
	ts, _, plain := newManifestsPATServer(t, []Scope{ScopeConfigRead}, []string{"cloud"})

	if code, _ := patDo(t, plain, http.MethodGet, ts.URL+"/api/pat/v1/config?scope=clowk&name=web", ""); code != http.StatusForbidden {
		t.Fatalf("config other scope: got %d, want 403", code)
	}
}

func TestPATManifests_PodRestartLimitedToResourceScopes(t *testing.T) {
	ts, _, plain := newManifestsPATServer(t, []Scope{ScopeActions}, []string{"cloud"})

	if code, _ := patDo(t, plain, http.MethodPost, ts.URL+"/api/pat/v1/pods/clowk-web.a3f9/restart", ""); code != http.StatusForbidden {
		t.Fatalf("restart pod of other scope: got %d, want 403", code)
	}
}

func TestPATPolicy(t *testing.T) {
	cases := []struct {
		name string
		m    string
		ok   bool
	}{
		{"loopback port and volume claim", cloudStatefulset, true},
		{"public port", `{"kind":"statefulset","scope":"cloud","name":"a","spec":{"image":"x","ports":["7419"]}}`, false},
		{"all interfaces port", `{"kind":"statefulset","scope":"cloud","name":"a","spec":{"image":"x","ports":["0.0.0.0:7419:7419"]}}`, false},
		{"host volume", `{"kind":"deployment","scope":"cloud","name":"a","spec":{"image":"x","volumes":["/:/host"]}}`, false},
		{"host network", `{"kind":"deployment","scope":"cloud","name":"a","spec":{"image":"x","network_mode":"host"}}`, false},
		{"docker options", `{"kind":"deployment","scope":"cloud","name":"a","spec":{"image":"x","docker_options":["--privileged"]}}`, false},
		{"init docker options", `{"kind":"deployment","scope":"cloud","name":"a","spec":{"image":"x","inits":[{"name":"i","image":"x","docker_options":["--privileged"]}]}}`, false},
		{"build", `{"kind":"deployment","scope":"cloud","name":"a","spec":{"build":{"context":"."}}}`, false},
		{"no image", `{"kind":"deployment","scope":"cloud","name":"a","spec":{}}`, false},
		{"plugin kind", `{"kind":"postgres","scope":"cloud","name":"a","spec":{}}`, false},
		{"job kind", `{"kind":"job","scope":"cloud","name":"a","spec":{"image":"x"}}`, false},
		{"tcp ingress", `{"kind":"ingress","scope":"cloud","name":"a","spec":{"host":"a.eu-de-1.anway.clowk.in","service":"a","port":7419,"tcp":{"listen":7419},"tls":{"enabled":true,"provider":"letsencrypt"}}}`, true},
		{"on-demand ingress", `{"kind":"ingress","scope":"cloud","name":"a","spec":{"host":"*.a.in","service":"a","port":80,"tls":{"enabled":true,"on_demand":true,"ask":"http://x"}}}`, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			manifests, err := decodeManifests([]byte(c.m))
			if err != nil {
				t.Fatal(err)
			}

			err = validatePATManifests(&PAT{ResourceScopes: []string{"cloud"}}, manifests)

			if c.ok && err != nil {
				t.Fatalf("want accepted, got %v", err)
			}

			if !c.ok && err == nil {
				t.Fatal("want refused, got accepted")
			}
		})
	}
}

func TestPATAllowsResourceScope(t *testing.T) {
	all := &PAT{}

	if !all.AllowsResourceScope("anything") {
		t.Fatal("a PAT without resource scopes reaches every scope")
	}

	cloud := &PAT{ResourceScopes: []string{"cloud"}}

	if !cloud.AllowsResourceScope("cloud") || cloud.AllowsResourceScope("clowk") {
		t.Fatal("a scoped PAT reaches only its scopes")
	}
}
