package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// redactAssetContent returns a copy of an asset spec with file
// content fields replaced by deterministic placeholders
// (sha256-prefix + size in bytes). The diff renderer consumes
// the redacted spec so terminal output never carries raw bytes
// from configs that may include hashed passwords, certs, ACLs,
// or other sensitive material.
//
// Behavior per source kind:
//
//   - file:   `{_source, content (base64), filename}` → content
//             becomes `<sha256:abcd1234 N bytes>`. The byte count
//             is taken from the DECODED bytes (what actually
//             ends up on disk), not the base64 envelope size.
//             Filename and source kind pass through.
//   - url:    `{_source, url}` passes through verbatim — URLs
//             are metadata operators want to see change. If
//             the operator embeds a credential in the URL,
//             that's a separate problem worth its own redact
//             pass; today we don't.
//   - inline: a plain string at `files.<key>` becomes the same
//             `<sha256:… N bytes>` summary.
//
// The function is a pure transform on json.RawMessage. On any
// decode error it returns the original spec untouched —
// "redaction" is a polish step, never a correctness step, so
// failing closed (rendering raw) beats failing loudly here.
func redactAssetContent(spec json.RawMessage) json.RawMessage {
	if len(spec) == 0 {
		return spec
	}

	var raw map[string]any
	if err := json.Unmarshal(spec, &raw); err != nil {
		return spec
	}

	files, ok := raw["files"].(map[string]any)
	if !ok {
		return spec
	}

	for key, val := range files {
		files[key] = redactAssetEntry(val)
	}

	out, err := json.Marshal(raw)
	if err != nil {
		return spec
	}

	return out
}

// redactAssetEntry rewrites one (key → source) pair. Centralised
// so the per-source-kind logic stays in one switch.
func redactAssetEntry(v any) any {
	switch v := v.(type) {
	case string:
		// Inline source — the value IS the bytes. Replace
		// with a summary string; downstream diff treats it
		// as a regular string field, so the user sees the
		// summary verbatim instead of the raw value.
		return assetContentSummary([]byte(v))

	case map[string]any:
		src, _ := v["_source"].(string)

		switch src {
		case "file":
			content, _ := v["content"].(string)

			// Decode best-effort. base64 errors fall back
			// to summarising the encoded form — operator
			// still gets a hash, just one tier removed.
			decoded, err := base64.StdEncoding.DecodeString(content)
			if err != nil {
				decoded = []byte(content)
			}

			return map[string]any{
				"_source":  "file",
				"filename": v["filename"],
				"content":  assetContentSummary(decoded),
			}

		case "url":
			// URL passes through — it's metadata, the diff
			// SHOULD show URL rotation / change.
			return v
		}
	}

	return v
}

// assetContentSummary renders a one-line placeholder identifying
// the bytes by their sha256 prefix and total length. 8 hex chars
// (32 bits of sha256) is enough to spot an unintended change
// while keeping the line readable; full hash would dominate a
// terminal column.
func assetContentSummary(b []byte) string {
	sum := sha256.Sum256(b)
	return fmt.Sprintf("<sha256:%s %d bytes>", hex.EncodeToString(sum[:4]), len(b))
}
