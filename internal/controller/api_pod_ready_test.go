// Tests for GET /pods/{name}/ready — the per-replica readiness
// endpoint caddy / operators query to decide routing.

package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeReadinessLookup is the test seam for ReadinessLookup.
// Maps container name → canned ReplicaReadinessStatus + presence
// flag so tests can simulate "ready", "not ready", and "no entry".
type fakeReadinessLookup struct {
	statuses map[string]ReplicaReadinessStatus
}

func (f *fakeReadinessLookup) LookupReadiness(name string) (ReplicaReadinessStatus, bool) {
	if f == nil || f.statuses == nil {
		return ReplicaReadinessStatus{}, false
	}

	s, ok := f.statuses[name]
	return s, ok
}

// TestHandlePodReady_Ready200 pins the happy path: a ready
// replica returns 200 with the full status envelope so caddy
// can route to it.
func TestHandlePodReady_Ready200(t *testing.T) {
	a := &API{
		Readiness: &fakeReadinessLookup{statuses: map[string]ReplicaReadinessStatus{
			"prod-api.a3f9": {
				ContainerName: "prod-api.a3f9",
				ReplicaID:     "a3f9",
				Ready:         true,
				StartupPassed: true,
			},
		}},
	}

	srv := httptest.NewServer(a.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/pods/prod-api.a3f9/ready")
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status=%d, want 200", resp.StatusCode)
	}

	var env envelope

	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatal(err)
	}

	if env.Status != "ok" {
		t.Errorf("envelope status=%q, want ok", env.Status)
	}
}

// TestHandlePodReady_NotReady503 pins the gating path: a not-
// ready replica returns 503 so caddy bypasses it as an upstream.
// Body still carries the status (Reason field) so the operator
// can see WHY it's unready.
func TestHandlePodReady_NotReady503(t *testing.T) {
	a := &API{
		Readiness: &fakeReadinessLookup{statuses: map[string]ReplicaReadinessStatus{
			"prod-api.a3f9": {
				ContainerName:  "prod-api.a3f9",
				Ready:          false,
				StartupPassed:  false,
				ReadinessPhase: "unhealthy",
				Reason:         "GET /healthz → 502",
			},
		}},
	}

	srv := httptest.NewServer(a.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/pods/prod-api.a3f9/ready")
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status=%d, want 503", resp.StatusCode)
	}
}

// TestHandlePodReady_Unknown404 verifies that a container the
// registry never knew about returns 404. Distinct from 503 so
// caddy can tell "not running yet" from "running but unhealthy".
func TestHandlePodReady_Unknown404(t *testing.T) {
	a := &API{Readiness: &fakeReadinessLookup{}}

	srv := httptest.NewServer(a.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/pods/nobody/ready")
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status=%d, want 404", resp.StatusCode)
	}
}

// TestHandlePodReady_NoLookup503 pins the "registry not wired"
// path. A test-mode API with no Readiness field set must surface
// 503, not panic.
func TestHandlePodReady_NoLookup503(t *testing.T) {
	a := &API{}

	srv := httptest.NewServer(a.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/pods/whatever/ready")
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status=%d, want 503", resp.StatusCode)
	}
}

// TestHandlePodReady_HostileName400 guards against path tricks
// like ../ or whitespace-injected names that could otherwise
// reach the lookup with surprising values.
func TestHandlePodReady_HostileName400(t *testing.T) {
	a := &API{Readiness: &fakeReadinessLookup{}}

	srv := httptest.NewServer(a.Handler())
	defer srv.Close()

	// http.NewRequest is needed because Get URL-encodes the
	// path; we want the raw bytes through the muxer.
	req, _ := http.NewRequest("GET", srv.URL+"/pods/.hidden/ready", nil)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status=%d, want 400 (hostile name)", resp.StatusCode)
	}
}
