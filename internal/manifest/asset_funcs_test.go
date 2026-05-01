package manifest

import (
	"encoding/json"
	"testing"
)

// TestUrlFunc_NoOpts pins the bare-call shape: `url("https://...")`
// produces a wire object with empty timeout / on_failure so the
// server's defaults take over. Operators who don't care write
// the one-arg form and get the existing behaviour.
func TestUrlFunc_NoOpts(t *testing.T) {
	src := `
asset "data" "redis" {
  acls = url("https://example.com/users.acl")
}
`
	tmp := writeTemp(t, "url-no-opts.hcl", src)

	mans, err := ParseFile(tmp, nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	files := extractAssetFiles(t, mans[0].Spec)

	urlObj, ok := files["acls"].(map[string]any)
	if !ok {
		t.Fatalf("acls not an object: %T", files["acls"])
	}

	if urlObj["_source"] != "url" {
		t.Errorf("_source: %v", urlObj["_source"])
	}

	if urlObj["url"] != "https://example.com/users.acl" {
		t.Errorf("url: %v", urlObj["url"])
	}

	// Empty options pass through as empty strings — server
	// applies defaults.
	if urlObj["timeout"] != "" {
		t.Errorf("timeout default: want empty, got %v", urlObj["timeout"])
	}

	if urlObj["on_failure"] != "" {
		t.Errorf("on_failure default: want empty, got %v", urlObj["on_failure"])
	}
}

// TestUrlFunc_WithOpts confirms the two-arg form parses
// correctly and the wire object carries the operator's
// timeout + on_failure values verbatim.
func TestUrlFunc_WithOpts(t *testing.T) {
	src := `
asset "data" "cdn" {
  acls = url("https://r2.example.com/users.acl", {
    timeout    = "5s"
    on_failure = "stale"
  })
}
`
	tmp := writeTemp(t, "url-with-opts.hcl", src)

	mans, err := ParseFile(tmp, nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	files := extractAssetFiles(t, mans[0].Spec)

	urlObj, ok := files["acls"].(map[string]any)
	if !ok {
		t.Fatalf("acls not an object: %T", files["acls"])
	}

	if urlObj["timeout"] != "5s" {
		t.Errorf("timeout: %v", urlObj["timeout"])
	}

	if urlObj["on_failure"] != "stale" {
		t.Errorf("on_failure: %v", urlObj["on_failure"])
	}
}

// TestUrlFunc_AllOnFailureValues confirms the parser doesn't
// reject any of the documented `on_failure` values. Validation
// of the value itself is server-side (so the parser stays
// permissive and a future plugin can add a new mode without
// touching the parser).
func TestUrlFunc_AllOnFailureValues(t *testing.T) {
	cases := []string{"error", "stale", "skip"}

	for _, mode := range cases {
		src := `
asset "data" "cdn" {
  k = url("https://x", { on_failure = "` + mode + `" })
}
`
		tmp := writeTemp(t, "url-onfailure-"+mode+".hcl", src)

		mans, err := ParseFile(tmp, nil)
		if err != nil {
			t.Errorf("on_failure=%q: parse %v", mode, err)
			continue
		}

		files := extractAssetFiles(t, mans[0].Spec)

		urlObj, _ := files["k"].(map[string]any)
		if urlObj["on_failure"] != mode {
			t.Errorf("on_failure=%q: got %v", mode, urlObj["on_failure"])
		}
	}
}

// TestUrlFunc_PartialOpts: declaring only one option keeps the
// other empty. Lets operators set just timeout OR just
// on_failure without a noisy "default" boilerplate.
func TestUrlFunc_PartialOpts(t *testing.T) {
	src := `
asset "data" "cdn" {
  k = url("https://x", { timeout = "10s" })
}
`
	tmp := writeTemp(t, "url-partial.hcl", src)

	mans, err := ParseFile(tmp, nil)
	if err != nil {
		t.Fatal(err)
	}

	files := extractAssetFiles(t, mans[0].Spec)

	urlObj, _ := files["k"].(map[string]any)

	if urlObj["timeout"] != "10s" {
		t.Errorf("timeout: %v", urlObj["timeout"])
	}

	if urlObj["on_failure"] != "" {
		t.Errorf("on_failure default: %v", urlObj["on_failure"])
	}
}

// extractAssetFiles unmarshals an asset manifest's spec JSON
// and returns the `files` map for direct inspection. Used by
// the url() function tests so the inner shape can be asserted
// without re-implementing the JSON walk in each one.
func extractAssetFiles(t *testing.T, spec []byte) map[string]any {
	t.Helper()

	var s struct {
		Files map[string]any `json:"files"`
	}

	if err := json.Unmarshal(spec, &s); err != nil {
		t.Fatalf("decode spec: %v", err)
	}

	return s.Files
}
