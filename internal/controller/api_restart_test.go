package controller

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeRestarter stubs DeploymentRestarter. Records the (scope, name)
// it received so tests can assert the API forwarded the resolved
// tuple correctly. Returning a stub error covers the failure path.
type fakeRestarter struct {
	gotScope    string
	gotName     string
	gotVerb     string
	gotTargetID string

	err         error
	newIDReturn string
}

func (f *fakeRestarter) Restart(_ context.Context, scope, name string) error {
	f.gotScope = scope
	f.gotName = name
	f.gotVerb = "restart"

	return f.err
}

func (f *fakeRestarter) Release(_ context.Context, scope, name string, _ io.Writer) error {
	f.gotScope = scope
	f.gotName = name
	f.gotVerb = "release"

	return f.err
}

func (f *fakeRestarter) Rollback(_ context.Context, scope, name, targetID string) (string, error) {
	f.gotScope = scope
	f.gotName = name
	f.gotVerb = "rollback"
	f.gotTargetID = targetID

	return f.newIDReturn, f.err
}

// TestRestart_DispatchesToDeploymentHandler confirms the happy path:
// /restart?kind=deployment&scope=&name= reaches the DeploymentRestarter
// with the tuple intact. Without this, the API could resolve scope
// wrong and restart a different app.
func TestRestart_DispatchesToDeploymentHandler(t *testing.T) {
	store := newMemStore()
	rs := &fakeRestarter{}

	api := &API{Store: store, Version: "test", Deployments: rs}

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/restart?kind=deployment&scope=clowk-lp&name=web",
		"application/json", nil)
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want 200", resp.StatusCode)
	}

	if rs.gotScope != "clowk-lp" || rs.gotName != "web" {
		t.Errorf("restarter got scope=%q name=%q, want clowk-lp/web",
			rs.gotScope, rs.gotName)
	}
}

// TestRestart_DefaultsKindToDeployment locks the ergonomic default:
// `kind` query is optional, defaults to deployment. The CLI sends
// it explicitly today, but a future shorthand (`/restart?name=web`)
// or a third-party caller benefits from the default.
func TestRestart_DefaultsKindToDeployment(t *testing.T) {
	store := newMemStore()
	rs := &fakeRestarter{}

	api := &API{Store: store, Version: "test", Deployments: rs}

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/restart?scope=clowk-lp&name=web",
		"application/json", nil)
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status=%d want 200 (kind should default to deployment)", resp.StatusCode)
	}
}

// TestRestart_RejectsNonDeploymentKinds protects the M-5 scope:
// jobs / cronjobs are transient (re-trigger via /jobs/run), and
// plugin-managed kinds don't fit rolling-replace. Future kinds
// will need to opt in here.
func TestRestart_RejectsNonDeploymentKinds(t *testing.T) {
	api := &API{Store: newMemStore(), Version: "test", Deployments: &fakeRestarter{}}

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/restart?kind=job&scope=ops&name=migrate",
		"application/json", nil)
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status=%d want 400", resp.StatusCode)
	}

	var env envelope
	_ = json.NewDecoder(resp.Body).Decode(&env)

	if !strings.Contains(env.Error, "deployment") {
		t.Errorf("error should mention deployment-only support: %q", env.Error)
	}
}

// TestRestart_ResolvesUnambiguousBareName mirrors the run paths:
// when scope is omitted and only one deployment in the store carries
// the requested name, the handler resolves it server-side. Without
// this, `vd restart web` would always 404.
func TestRestart_ResolvesUnambiguousBareName(t *testing.T) {
	store := newMemStore()
	rs := &fakeRestarter{}

	_, _ = store.Put(t.Context(), &Manifest{Kind: KindDeployment, Scope: "clowk-lp", Name: "web"})

	api := &API{Store: store, Version: "test", Deployments: rs}

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/restart?name=web", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want 200", resp.StatusCode)
	}

	if rs.gotScope != "clowk-lp" {
		t.Errorf("scope should auto-resolve to clowk-lp, got %q", rs.gotScope)
	}
}

// TestRestart_503WhenRestarterNotConfigured is the
// reduced-functionality contract: an API wired without Deployments
// returns 503 instead of nil-panicking.
func TestRestart_503WhenRestarterNotConfigured(t *testing.T) {
	api := &API{Store: newMemStore(), Version: "test"}

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/restart?scope=clowk-lp&name=web", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status=%d want 503", resp.StatusCode)
	}
}

// TestRestart_PropagatesHandlerError confirms the failure path:
// when DeploymentHandler.Restart returns an error (replicas missing,
// docker daemon down, etc.), the API surfaces it as 500 with the
// message verbatim. The CLI relies on this to print "no live
// replicas" and similar diagnostic text.
func TestRestart_PropagatesHandlerError(t *testing.T) {
	store := newMemStore()
	rs := &fakeRestarter{err: errors.New("no live replicas to restart")}

	api := &API{Store: store, Version: "test", Deployments: rs}

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/restart?kind=deployment&scope=clowk-lp&name=web",
		"application/json", nil)
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status=%d want 500", resp.StatusCode)
	}

	var env envelope
	_ = json.NewDecoder(resp.Body).Decode(&env)

	if !strings.Contains(env.Error, "no live replicas") {
		t.Errorf("error should surface verbatim, got %q", env.Error)
	}
}
