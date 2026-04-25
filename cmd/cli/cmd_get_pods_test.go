package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.voodu.clowk.in/internal/controller"
)

// TestGetPodsRequestsControllerEndpoint exercises the wire contract:
// the CLI hits GET /pods, optionally with the right query params, and
// the controller answer flows through unchanged.
func TestGetPodsRequestsControllerEndpoint(t *testing.T) {
	var (
		gotPath     string
		gotRawQuery string
	)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotRawQuery = r.URL.RawQuery

		resp := map[string]any{
			"status": "ok",
			"data": map[string]any{
				"pods": []controller.Pod{
					{
						Name: "softphone-web.a3f9", Kind: "deployment", Scope: "softphone",
						ResourceName: "web", ReplicaID: "a3f9", Image: "softphone-web:latest",
						Status: "Up 1 minute", Running: true,
					},
				},
			},
		}

		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()

	root := newRootCmd()
	_ = root.PersistentFlags().Set("controller-url", ts.URL)

	var stdout bytes.Buffer

	root.SetOut(&stdout)
	root.SetErr(&stdout)
	root.SetArgs([]string{"get", "pods", "--kind", "deployment", "--scope", "softphone"})

	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}

	if gotPath != "/pods" {
		t.Errorf("path=%q want /pods", gotPath)
	}

	if !strings.Contains(gotRawQuery, "kind=deployment") || !strings.Contains(gotRawQuery, "scope=softphone") {
		t.Errorf("raw query=%q missing filters", gotRawQuery)
	}
}

func TestGetPodsRendersTextTable(t *testing.T) {
	pods := []controller.Pod{
		{
			Name: "softphone-web.a3f9", Kind: "deployment", Scope: "softphone",
			ResourceName: "web", ReplicaID: "a3f9", Image: "softphone-web:latest",
			Status: "Up 2 hours", Running: true,
		},
		{
			Name: "softphone-web.bb01", Kind: "deployment", Scope: "softphone",
			ResourceName: "web", ReplicaID: "bb01", Image: "softphone-web:latest",
			Status: "Up 5 minutes", Running: true,
		},
	}

	var buf bytes.Buffer
	if err := renderPodsTable(&buf, pods); err != nil {
		t.Fatal(err)
	}

	out := buf.String()

	for _, want := range []string{
		"NAME", "KIND", "SCOPE", "RESOURCE", "IMAGE", "STATUS",
		"softphone-web.a3f9", "softphone-web.bb01", "deployment", "softphone",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing %q\n%s", want, out)
		}
	}
}

func TestGetPodsRendersLegacyKind(t *testing.T) {
	pods := []controller.Pod{
		{
			Name: "old-app", Image: "old:latest", Status: "Up 1 day", Running: true,
		},
	}

	var buf bytes.Buffer
	if err := renderPodsTable(&buf, pods); err != nil {
		t.Fatal(err)
	}

	out := buf.String()

	if !strings.Contains(out, "(legacy)") {
		t.Errorf("legacy pod missing (legacy) kind marker:\n%s", out)
	}

	if !strings.Contains(out, "old-app") {
		t.Errorf("legacy pod name missing:\n%s", out)
	}
}

func TestGetPodsEmptyMessage(t *testing.T) {
	var buf bytes.Buffer
	if err := renderPodsTable(&buf, nil); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(buf.String(), "No voodu-managed containers found") {
		t.Errorf("empty render missing helpful message: %q", buf.String())
	}
}
