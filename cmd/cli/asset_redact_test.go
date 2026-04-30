package main

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// TestRedactAssetContent_FileSource: bytes embedded via file()
// get summarised. Filename + _source pass through (operator
// wants to see filename rename or source-kind change in the
// diff). The summary is a stable sha256-prefix + size string.
func TestRedactAssetContent_FileSource(t *testing.T) {
	body := []byte("requirepass mysecretvalue\n")

	spec, _ := json.Marshal(map[string]any{
		"files": map[string]any{
			"redis_conf": map[string]any{
				"_source":  "file",
				"content":  base64.StdEncoding.EncodeToString(body),
				"filename": "redis.conf",
			},
		},
	})

	out := redactAssetContent(spec)

	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}

	files := got["files"].(map[string]any)
	entry := files["redis_conf"].(map[string]any)

	// Filename + source kind survive — operators want them in the diff.
	if entry["filename"] != "redis.conf" {
		t.Errorf("filename should pass through: %v", entry["filename"])
	}

	if entry["_source"] != "file" {
		t.Errorf("_source should pass through: %v", entry["_source"])
	}

	// Content gets the placeholder, NOT the raw secret.
	content := entry["content"].(string)

	if !strings.HasPrefix(content, "<sha256:") {
		t.Errorf("content not redacted: %q", content)
	}

	if strings.Contains(content, "mysecretvalue") {
		t.Errorf("REDACTION LEAKED secret content: %q", content)
	}

	if strings.Contains(content, base64.StdEncoding.EncodeToString(body)) {
		t.Errorf("REDACTION LEAKED base64 content: %q", content)
	}

	// Size reflects the DECODED bytes, not the base64 envelope.
	expectedSize := len(body)
	if !strings.Contains(content, "26 bytes") && expectedSize == 26 {
		t.Errorf("size summary missing or wrong: %q", content)
	}
}

// TestRedactAssetContent_InlineSource: plain string values
// get summarised in place. Common case for short literals
// (motd, single-line ACL entries).
func TestRedactAssetContent_InlineSource(t *testing.T) {
	spec, _ := json.Marshal(map[string]any{
		"files": map[string]any{
			"motd": "Welcome to production redis (do not abuse)",
		},
	})

	out := redactAssetContent(spec)

	var got map[string]any
	_ = json.Unmarshal(out, &got)

	files := got["files"].(map[string]any)
	motd := files["motd"].(string)

	if !strings.HasPrefix(motd, "<sha256:") {
		t.Errorf("inline value not summarised: %q", motd)
	}

	if strings.Contains(motd, "Welcome") {
		t.Errorf("REDACTION LEAKED inline value: %q", motd)
	}
}

// TestRedactAssetContent_URLSource: URL passes through verbatim.
// Operators want to see URL rotation / source change in the diff;
// the URL itself isn't bytes-sensitive (operator is responsible
// for not embedding credentials in the URL).
func TestRedactAssetContent_URLSource(t *testing.T) {
	spec, _ := json.Marshal(map[string]any{
		"files": map[string]any{
			"users_acl": map[string]any{
				"_source": "url",
				"url":     "https://r2.example.com/voodu/users.acl",
			},
		},
	})

	out := redactAssetContent(spec)

	var got map[string]any
	_ = json.Unmarshal(out, &got)

	files := got["files"].(map[string]any)
	entry := files["users_acl"].(map[string]any)

	if entry["url"] != "https://r2.example.com/voodu/users.acl" {
		t.Errorf("URL must pass through: %v", entry["url"])
	}
}

// TestRedactAssetContent_DeterministicSummary: the same bytes
// always produce the same placeholder string. Without this, a
// re-apply with identical content would diff falsely against
// itself (different summary every time = endless drift).
func TestRedactAssetContent_DeterministicSummary(t *testing.T) {
	a := assetContentSummary([]byte("hello world"))
	b := assetContentSummary([]byte("hello world"))

	if a != b {
		t.Errorf("summary not deterministic: %q vs %q", a, b)
	}

	c := assetContentSummary([]byte("hello WORLD"))

	if a == c {
		t.Errorf("summary collided across different inputs: %q", a)
	}
}

// TestRedactAssetContent_PreservesNonAssetSpec: the function is
// a no-op on specs that don't have a top-level `files` map.
// Defensive — the caller already gates on Kind == asset, but a
// mismatched call shouldn't corrupt the spec.
func TestRedactAssetContent_PreservesNonAssetSpec(t *testing.T) {
	spec := json.RawMessage(`{"image":"redis:8","replicas":1}`)

	out := redactAssetContent(spec)

	if string(out) != string(spec) {
		t.Errorf("non-asset spec mutated: %s → %s", spec, out)
	}
}

// TestRedactAssetContent_EmptySpec: nil / empty spec returns
// unchanged. Renderer paths that lack a current side (new
// resource creation) pass nil here.
func TestRedactAssetContent_EmptySpec(t *testing.T) {
	if got := redactAssetContent(nil); got != nil {
		t.Errorf("nil spec should return nil, got %s", got)
	}

	empty := json.RawMessage(``)

	if got := redactAssetContent(empty); string(got) != string(empty) {
		t.Errorf("empty spec mutated: %s", got)
	}
}
