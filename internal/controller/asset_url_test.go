package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestFetchAssetURLShared_Timeout pins the typed-error contract:
// when the GET hits the per-fetch deadline, fetchAssetURLShared
// returns *AssetURLTimeoutError with the URL + duration so the
// stamping layer can format an operator-friendly message.
func TestFetchAssetURLShared_Timeout(t *testing.T) {
	withTempVooduRoot(t)

	// Slow server that doesn't respond before the timeout fires.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			// Client gave up — tear down fast so the test
			// doesn't pay the full sleep on httptest.Close.
		case <-time.After(2 * time.Second):
		}
	}))
	defer srv.Close()

	_, err := fetchAssetURLShared(context.Background(), nil, nil, srv.URL, 100*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error")
	}

	var te *AssetURLTimeoutError
	if !errors.As(err, &te) {
		t.Fatalf("expected *AssetURLTimeoutError, got %T: %v", err, err)
	}

	if te.URL != srv.URL {
		t.Errorf("URL: %q want %q", te.URL, srv.URL)
	}

	if te.Timeout != 100*time.Millisecond {
		t.Errorf("Timeout: %v want 100ms", te.Timeout)
	}

	// Operator-facing message contains the URL, the duration,
	// and the override hint. Pin pieces of it so a refactor
	// doesn't accidentally drop the actionable parts.
	msg := te.Error()

	for _, want := range []string{srv.URL, "100ms", `url("…", { timeout = "<longer>" })`, `on_failure = "stale"`} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message missing %q:\n%s", want, msg)
		}
	}
}

// TestMaterializeAssetSpec_OnFailureError: when a url() declares
// on_failure="error" and the fetch fails, materialization
// returns a fatal error immediately — apply rejects without
// the silent stale-good fallback.
func TestMaterializeAssetSpec_OnFailureError(t *testing.T) {
	withTempVooduRoot(t)
	store := newMemStore()

	// Slow server.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			// Client gave up — tear down fast so the test
			// doesn't pay the full sleep on httptest.Close.
		case <-time.After(2 * time.Second):
		}
	}))
	defer srv.Close()

	spec, _ := json.Marshal(map[string]any{
		"files": map[string]any{
			"acls": map[string]any{
				"_source":    "url",
				"url":        srv.URL,
				"timeout":    "50ms",
				"on_failure": "error",
			},
		},
	})

	m := &Manifest{
		Kind:  KindAsset,
		Scope: "data",
		Name:  "cdn",
		Spec:  spec,
	}

	_, _, err := materializeAssetSpec(context.Background(), store, nil, nil, m)
	if err == nil {
		t.Fatal("expected fatal error from on_failure=error")
	}

	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error should mention timeout: %v", err)
	}

	if !strings.Contains(err.Error(), "asset/data/cdn/acls") {
		t.Errorf("error should identify the failing key: %v", err)
	}
}

// TestMaterializeAssetSpec_OnFailureStaleDefault: when a url()
// fetch fails and on_failure isn't set explicitly, the
// pipeline records the per-key error but keeps the apply
// going. Combined with the disk-cache fallback (existing
// stale-good recovery), the consumer still gets a working
// asset path on re-applies.
func TestMaterializeAssetSpec_OnFailureStaleDefault(t *testing.T) {
	withTempVooduRoot(t)
	store := newMemStore()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			// Client gave up — tear down fast so the test
			// doesn't pay the full sleep on httptest.Close.
		case <-time.After(2 * time.Second):
		}
	}))
	defer srv.Close()

	// No on_failure declared → defaults to "stale" semantics.
	spec, _ := json.Marshal(map[string]any{
		"files": map[string]any{
			"acls": map[string]any{
				"_source": "url",
				"url":     srv.URL,
				"timeout": "50ms",
			},
		},
	})

	m := &Manifest{Kind: KindAsset, Scope: "data", Name: "cdn", Spec: spec}

	digests, errs, err := materializeAssetSpec(context.Background(), store, nil, nil, m)
	// No fatal error — apply continues.
	if err != nil {
		t.Errorf("expected no fatal error in stale mode, got: %v", err)
	}

	// Per-key error is recorded.
	if errs["acls"] == "" {
		t.Errorf("per-key error should be recorded, got: %+v", errs)
	}

	// No digest because the fetch failed and there's no
	// disk-cached bytes from a prior apply (fresh tempdir).
	// The consumer phase will reject any ref to this key —
	// strict on first apply, tolerant on re-applies.
	if _, ok := digests["acls"]; ok {
		t.Errorf("digest should be absent on first-apply failure with no fallback")
	}
}

// TestResolveAssetSourceForStamping_TimeoutHonored: the
// per-url() timeout option flows through into the actual GET
// deadline — without this, the operator's `timeout = "5s"`
// would silently be ignored and the default 30s would apply.
func TestResolveAssetSourceForStamping_TimeoutHonored(t *testing.T) {
	withTempVooduRoot(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
	}))
	defer srv.Close()

	raw, _ := json.Marshal(map[string]any{
		"_source": "url",
		"url":     srv.URL,
		"timeout": "50ms",
	})

	start := time.Now()
	_, opts, err := resolveAssetSourceForStamping(context.Background(), nil, raw)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error")
	}

	// Must have given up well before the server's 500ms sleep
	// — proving the 50ms operator timeout was respected.
	if elapsed > 200*time.Millisecond {
		t.Errorf("elapsed %v: timeout should have fired around 50ms", elapsed)
	}

	// Default on_failure is "stale".
	if opts.OnFailure != OnFailureStale {
		t.Errorf("default on_failure: %q want %q", opts.OnFailure, OnFailureStale)
	}
}

// TestResolveAssetSourceForStamping_ParsesOnFailure pins the
// option round-trip: HCL's `on_failure = "error"` arrives in
// the source object as a string, gets parsed into
// sourceOptions.OnFailure verbatim.
func TestResolveAssetSourceForStamping_ParsesOnFailure(t *testing.T) {
	withTempVooduRoot(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	cases := []string{"error", "stale", "skip"}

	for _, mode := range cases {
		raw, _ := json.Marshal(map[string]any{
			"_source":    "url",
			"url":        srv.URL,
			"on_failure": mode,
		})

		_, opts, err := resolveAssetSourceForStamping(context.Background(), nil, raw)
		if err != nil {
			t.Errorf("on_failure=%q: %v", mode, err)
			continue
		}

		if opts.OnFailure != mode {
			t.Errorf("on_failure=%q got %q", mode, opts.OnFailure)
		}
	}
}

// TestParseDurationOrZero pins the permissive parser used to
// translate the JSON-wire timeout value into a time.Duration.
// Garbage / missing → 0 (server falls back to default), valid
// strings → parsed duration.
func TestParseDurationOrZero(t *testing.T) {
	cases := []struct {
		in   any
		want time.Duration
	}{
		{nil, 0},
		{"", 0},
		{"5s", 5 * time.Second},
		{"100ms", 100 * time.Millisecond},
		{"2m", 2 * time.Minute},
		{"garbage", 0},
		{42, 0}, // wrong type — defensive
	}

	for _, tc := range cases {
		got := parseDurationOrZero(tc.in)
		if got != tc.want {
			t.Errorf("parseDurationOrZero(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
