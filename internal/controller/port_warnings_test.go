package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"go.voodu.clowk.in/internal/activity"
	"go.voodu.clowk.in/internal/plugins"
	"go.voodu.clowk.in/internal/triggerspec"
	"go.voodu.clowk.in/pkg/plugin"
)

func portManifest(kind Kind, ports ...string) *Manifest {
	spec, _ := json.Marshal(map[string]any{"image": "x:1", "ports": ports})

	return &Manifest{Kind: kind, Scope: "contagorda", Name: "cache", Spec: spec}
}

// decodeApplyWarnings reads data.warnings out of an /apply response.
func decodeApplyWarnings(t *testing.T, resp *http.Response) []string {
	t.Helper()

	var env struct {
		Data struct {
			Warnings []string `json:"warnings"`
		} `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatal(err)
	}

	return env.Data.Warnings
}

// A host port docker is free to move is what gets caught — but only off
// loopback. On loopback nothing outside the host reached it, and ingress
// dials the container by name, so the default `ports = ["80"]` stays quiet.
func TestEmptyHostPortWarnsOnlyOffLoopback(t *testing.T) {
	for _, tc := range []struct {
		port string
		warn bool
	}{
		{"10.8.0.1::6379", true},
		{"0.0.0.0::80", true},
		{"::80", true},
		{"[::]::80", true},
		{"10.8.0.1::53/udp", true},
		{"80", false},
		{"80/udp", false},
		{"127.0.0.1::80", false},
		{"127.0.0.2::80", false},
		{"[::1]::80", false},
		{"3000:80", false},
		{"10.8.0.1:6379:6379", false},
	} {
		t.Run(tc.port, func(t *testing.T) {
			got := emptyHostPortWarnings([]*Manifest{portManifest(KindStatefulset, tc.port)})

			if (len(got) > 0) != tc.warn {
				t.Errorf("ports=[%q]: warnings = %v, want a warning: %v", tc.port, got, tc.warn)
			}
		})
	}
}

// The operator has to act on the message, so it names the resource, points
// at the entry, repeats what they wrote, and hands them the fix.
func TestEmptyHostPortWarningCarriesTheFix(t *testing.T) {
	for _, tc := range []struct {
		port string
		fix  string
	}{
		{"10.8.0.1::6379", `"10.8.0.1:6379:6379"`},
		{"10.8.0.1::53/udp", `"10.8.0.1:53:53/udp"`},
		{"[::]::80", `"[::]:80:80"`},
	} {
		got := emptyHostPortWarnings([]*Manifest{portManifest(KindStatefulset, "8080", tc.port)})

		if len(got) != 1 {
			t.Fatalf("ports=[8080 %q]: want one warning, got %v", tc.port, got)
		}

		for _, want := range []string{"statefulset/contagorda/cache", "ports[1]", strconv.Quote(tc.port), "Pin it: " + tc.fix} {
			if !strings.Contains(got[0], want) {
				t.Errorf("warning %q is missing %q", got[0], want)
			}
		}
	}
}

// Only the kinds that publish host ports are read.
func TestEmptyHostPortReadsOnlyKindsThatPublish(t *testing.T) {
	for _, tc := range []struct {
		kind Kind
		warn bool
	}{
		{KindDeployment, true},
		{KindStatefulset, true},
		{KindJob, false},
		{KindIngress, false},
	} {
		got := emptyHostPortWarnings([]*Manifest{portManifest(tc.kind, "10.8.0.1::6379")})

		if (len(got) > 0) != tc.warn {
			t.Errorf("%s: warnings = %v, want a warning: %v", tc.kind, got, tc.warn)
		}
	}
}

// A warning, not a rejection: the manifest is stored, and the operator hears
// about it in the response.
func TestApplyWarnsAboutAnEmptyHostPortAndStillApplies(t *testing.T) {
	api, store := newTestAPI(t)
	ts := httptest.NewServer(api.Handler())

	defer ts.Close()

	resp := postBody(t, ts.URL+"/apply", `{"kind":"statefulset","scope":"contagorda","name":"cache","spec":{"image":"redis:7","ports":["10.8.0.1::6379"]}}`)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a warning must not fail the apply: status %d", resp.StatusCode)
	}

	warnings := decodeApplyWarnings(t, resp)

	if len(warnings) != 1 || !strings.Contains(warnings[0], "statefulset/contagorda/cache") {
		t.Fatalf("warnings = %v", warnings)
	}

	got, err := store.Get(t.Context(), KindStatefulset, "contagorda", "cache")
	if err != nil || got == nil {
		t.Fatalf("a warning must not stop the manifest being stored: %v", err)
	}
}

// `vd diff` must show what the apply would flag, or the warning is only ever
// seen after the fact.
func TestDryRunCarriesTheWarning(t *testing.T) {
	api, store := newTestAPI(t)
	ts := httptest.NewServer(api.Handler())

	defer ts.Close()

	resp := postBody(t, ts.URL+"/apply?dry_run=true", `{"kind":"deployment","scope":"contagorda","name":"api","spec":{"image":"x:1","ports":["0.0.0.0::80"]}}`)
	defer resp.Body.Close()

	if warnings := decodeApplyWarnings(t, resp); len(warnings) != 1 {
		t.Fatalf("warnings = %v", warnings)
	}

	if got, _ := store.Get(t.Context(), KindDeployment, "contagorda", "api"); got != nil {
		t.Fatal("dry run stored the manifest")
	}
}

// A clean batch reads exactly as before: no `warnings` key at all.
func TestCleanApplyHasNoWarningsKey(t *testing.T) {
	api, _ := newTestAPI(t)
	ts := httptest.NewServer(api.Handler())

	defer ts.Close()

	resp := postBody(t, ts.URL+"/apply", `{"kind":"statefulset","scope":"contagorda","name":"cache","spec":{"image":"redis:7","ports":["10.8.0.1:6379:6379"]}}`)
	defer resp.Body.Close()

	var env struct {
		Data map[string]json.RawMessage `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatal(err)
	}

	if _, ok := env.Data["warnings"]; ok {
		t.Errorf("clean apply carries a warnings key: %s", env.Data["warnings"])
	}
}

// The incident was a plugin block — `redis "contagorda" "cache"` with the
// port inside it. That `ports` only exists after the plugin expands the block
// into a statefulset, which is why the check runs on the expanded batch.
func TestApplyWarnsForAPluginExpandedStatefulset(t *testing.T) {
	api, _ := newTestAPI(t)

	dir := t.TempDir()
	expand := dir + "/expand"

	writePluginScript(t, expand, `#!/usr/bin/env bash
cat >/dev/null
echo '{"status":"ok","data":{"kind":"statefulset","scope":"contagorda","name":"cache","spec":{"image":"redis:7","ports":["10.8.0.1::6379"]}}}'
`)

	api.PluginBlocks = &fakePluginRegistry{plugins: map[string]*plugins.LoadedPlugin{
		"redis": {Manifest: plugin.Manifest{Name: "redis"}, Dir: dir, Commands: map[string]string{"expand": expand}},
	}}

	ts := httptest.NewServer(api.Handler())

	defer ts.Close()

	resp := postBody(t, ts.URL+"/apply", `{"kind":"redis","scope":"contagorda","name":"cache","spec":{}}`)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}

	warnings := decodeApplyWarnings(t, resp)

	if len(warnings) != 1 || !strings.Contains(warnings[0], "statefulset/contagorda/cache") {
		t.Fatalf("warnings = %v", warnings)
	}
}

// The trail carries the warning next to the apply that caused it.
func TestApplyRecordsTheWarningOnTheTrail(t *testing.T) {
	api, _, dir := newActivityAPI(t)
	ts := httptest.NewServer(api.Handler())

	defer ts.Close()

	resp := postBody(t, ts.URL+"/apply", `{"kind":"statefulset","scope":"contagorda","name":"cache","spec":{"image":"redis:7","ports":["10.8.0.1::6379"]}}`)
	resp.Body.Close()

	recs := trail(t, dir)
	finished := recs[len(recs)-1]

	if finished.Event != activity.EventFinished {
		t.Fatalf("last record is %q, want finished", finished.Event)
	}

	if len(finished.Warnings) != 1 || !strings.Contains(finished.Warnings[0], "statefulset/contagorda/cache") {
		t.Fatalf("trail warnings = %v", finished.Warnings)
	}
}

// A deploy from GitHub discards the apply's response when it succeeds, so the
// trail is the only place its warning can land. It goes through the same
// in-process apply, which is what this pins.
func TestDeployPlaneApplyLeavesTheWarningOnTheTrail(t *testing.T) {
	api, _, dir := newActivityAPI(t)

	manifests := []Manifest{{
		Kind:  KindStatefulset,
		Scope: "contagorda",
		Name:  "cache",
		Spec:  json.RawMessage(`{"image":"redis:7","ports":["10.8.0.1::6379"]}`),
	}}

	err := api.applyManifestsInProcess(
		httptest.NewRecorder(),
		httptest.NewRequest(http.MethodPost, "/deploy", nil),
		&Trigger{ID: "t1"},
		deployRunRequest{SHA: "a1b2c3d"},
		triggerspec.Spec{Name: "cache", Apply: triggerspec.Apply{File: "cache.hcl"}},
		manifests,
	)
	if err != nil {
		t.Fatalf("a warning must not fail the deploy: %v", err)
	}

	recs := trail(t, dir)
	finished := recs[len(recs)-1]

	if finished.Origin != activity.OriginDeployPlane {
		t.Errorf("origin = %q, want the deploy plane", finished.Origin)
	}

	if len(finished.Warnings) != 1 {
		t.Fatalf("trail warnings = %v", finished.Warnings)
	}
}
