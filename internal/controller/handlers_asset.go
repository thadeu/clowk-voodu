package controller

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.voodu.clowk.in/internal/paths"
)

// AssetHandler reconciles `asset { … }` manifests by
// materialising every (key → source) pair onto the host
// filesystem under `<assets_root>/<scope>/<name>/<key>`.
// Materialised paths are what `${asset.<name>.<key>}`
// interpolation resolves to in deployment / statefulset
// volumes / commands / env strings.
//
// Sources:
//
//   - "file"   — bytes embedded in the manifest by the CLI
//                (base64). Just decode + write.
//   - "url"    — server fetches at reconcile time, caches by
//                ETag/Last-Modified under <root>/cache/.
//                Re-applies that didn't change the URL skip
//                the network round-trip.
//   - "inline" — plain string in the manifest spec; written
//                verbatim. Tagged here as "inline" for
//                consistency; on the wire it's just a JSON
//                string (no _source field).
//
// On every Apply the handler re-materialises every key.
// On Delete it removes the asset directory wholesale. The
// /status blob carries the per-key sha256 so consumers
// (resources interpolating `${asset.X.Y}`) can fold it into
// their spec hash and trigger rolling restart on content
// drift — see resolveAssetRefs in M-C3/M-C4.
type AssetHandler struct {
	Store Store
	Log   *log.Logger

	// HTTP is the client used for `url()` sources. Nil falls
	// back to a default client with a generous timeout. Tests
	// inject a stub to avoid real network calls.
	HTTP *http.Client
}

// AssetStatus is the persisted shape of /status/assets/<scope>-<name>.
// Keys map to a per-file digest the controller computed at
// materialisation time. Resources that fold this into their
// spec hash get a deterministic "config drift triggers
// restart" without re-reading the filesystem on every
// reconcile — they read /status once and trust the digest.
type AssetStatus struct {
	// Files maps `key → sha256(content)`. Ordered keys would
	// be marshal-deterministic anyway (Go maps marshal
	// alphabetically since 1.12), so this stays a plain map.
	Files map[string]string `json:"files,omitempty"`

	// MaterialisedAt is the wall-clock time of the most
	// recent successful materialisation. Useful for
	// debugging "did the server pick up my new R2 URL
	// content?" without diff-ing the file content.
	MaterialisedAt time.Time `json:"materialised_at,omitempty"`
}

// Handle dispatches per WatchEvent type. Mirrors the shape
// every other reconciler handler uses — small, predictable.
func (h *AssetHandler) Handle(ctx context.Context, ev WatchEvent) error {
	switch ev.Type {
	case WatchPut:
		return h.apply(ctx, ev)

	case WatchDelete:
		return h.remove(ctx, ev)
	}

	return nil
}

// assetSpec mirrors manifest.AssetSpec — the controller
// re-decodes the wire JSON. Defined locally to avoid the
// reverse import (manifest already imports controller).
type assetSpec struct {
	Files map[string]json.RawMessage `json:"files,omitempty"`
}

func (h *AssetHandler) apply(ctx context.Context, ev WatchEvent) error {
	if ev.Manifest == nil {
		return fmt.Errorf("put event without manifest")
	}

	var spec assetSpec

	if len(ev.Manifest.Spec) == 0 {
		return fmt.Errorf("asset/%s/%s: empty spec", ev.Scope, ev.Name)
	}

	if err := json.Unmarshal(ev.Manifest.Spec, &spec); err != nil {
		return fmt.Errorf("decode asset spec: %w", err)
	}

	dir := paths.AssetDir(ev.Scope, ev.Name)

	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create asset dir: %w", err)
	}

	digests := make(map[string]string, len(spec.Files))

	// Track the keys we wrote on this reconcile so we can
	// drop any leftover files from a previous version of the
	// asset (operator removed a key — file shouldn't linger).
	written := make(map[string]bool, len(spec.Files))

	for key, raw := range spec.Files {
		if !validAssetKey(key) {
			return fmt.Errorf("asset/%s/%s: key %q must be alphanumeric + underscore + hyphen (no dots, no whitespace)", ev.Scope, ev.Name, key)
		}

		bytes, err := h.resolveSource(ctx, raw)
		if err != nil {
			return fmt.Errorf("asset/%s/%s/%s: %w", ev.Scope, ev.Name, key, err)
		}

		dst := paths.AssetFile(ev.Scope, ev.Name, key)

		if err := atomicWrite(dst, bytes); err != nil {
			return fmt.Errorf("write %s: %w", dst, err)
		}

		sum := sha256.Sum256(bytes)
		digests[key] = hex.EncodeToString(sum[:])
		written[key] = true
	}

	// Sweep stale files: anything in the asset dir that the
	// new spec didn't ask for gets removed. Without this, a
	// renamed key would leave the old file mounted on the
	// container indefinitely.
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}

			if !written[e.Name()] {
				_ = os.Remove(filepath.Join(dir, e.Name()))
			}
		}
	}

	status := AssetStatus{
		Files:          digests,
		MaterialisedAt: time.Now().UTC(),
	}

	blob, err := json.Marshal(status)
	if err != nil {
		return err
	}

	if err := h.Store.PutStatus(ctx, KindAsset, AppID(ev.Scope, ev.Name), blob); err != nil {
		return err
	}

	h.logf("asset/%s/%s materialised %d file(s) at %s", ev.Scope, ev.Name, len(digests), dir)

	return nil
}

// remove tears down the asset directory and clears status.
// Resources that interpolated `${asset.X.Y}` and are still
// running keep their pre-existing bind mounts pointing at the
// (now-deleted) path — docker doesn't unmount on file removal.
// On the next reconcile of those resources the
// resolveAssetRefs path will fail loudly because the
// referenced asset is gone, prompting the operator to remove
// the dangling references.
func (h *AssetHandler) remove(ctx context.Context, ev WatchEvent) error {
	dir := paths.AssetDir(ev.Scope, ev.Name)

	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("remove asset dir: %w", err)
	}

	if err := h.Store.DeleteStatus(ctx, KindAsset, AppID(ev.Scope, ev.Name)); err != nil {
		return fmt.Errorf("clear asset status: %w", err)
	}

	h.logf("asset/%s/%s removed", ev.Scope, ev.Name)

	return nil
}

// resolveSource decodes one entry from the asset spec. The
// wire shape is heterogeneous: a plain JSON string is an
// inline literal; a JSON object with `_source: "file"` carries
// base64 bytes; a JSON object with `_source: "url"` is fetched
// server-side. Anything else is a parser bug — surface
// loudly.
func (h *AssetHandler) resolveSource(ctx context.Context, raw json.RawMessage) ([]byte, error) {
	trimmed := strings.TrimSpace(string(raw))

	if len(trimmed) == 0 {
		return nil, fmt.Errorf("empty source")
	}

	if trimmed[0] == '"' {
		// Plain string → inline source.
		var s string

		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, fmt.Errorf("decode inline string: %w", err)
		}

		return []byte(s), nil
	}

	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("decode source object: %w", err)
	}

	src, _ := obj["_source"].(string)

	switch src {
	case "file":
		content, ok := obj["content"].(string)
		if !ok {
			return nil, fmt.Errorf(`file source missing "content" string`)
		}

		decoded, err := base64.StdEncoding.DecodeString(content)
		if err != nil {
			return nil, fmt.Errorf("file source: invalid base64: %w", err)
		}

		return decoded, nil

	case "url":
		u, ok := obj["url"].(string)
		if !ok {
			return nil, fmt.Errorf(`url source missing "url" string`)
		}

		return h.fetchURL(ctx, u)

	default:
		return nil, fmt.Errorf("unknown asset source %q (want file|url|inline)", src)
	}
}

// fetchURL retrieves the URL with a small ETag-based cache so
// re-applies that don't change content skip the network. The
// cache lives under <root>/cache/<sha256-of-url> with two
// sibling files: `.bytes` (the response body) and `.meta`
// (JSON with the ETag and Last-Modified the server sent
// last). Any cache miss / stale entry triggers a fresh GET.
func (h *AssetHandler) fetchURL(ctx context.Context, u string) ([]byte, error) {
	client := h.HTTP
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}

	cacheKey := sha256OfString(u)
	cacheDir := paths.CacheDir()

	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return nil, fmt.Errorf("ensure cache dir: %w", err)
	}

	bytesPath := filepath.Join(cacheDir, cacheKey+".bytes")
	metaPath := filepath.Join(cacheDir, cacheKey+".meta")

	prev, _ := readCacheMeta(metaPath)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}

	if prev.ETag != "" {
		req.Header.Set("If-None-Match", prev.ETag)
	}

	if prev.LastModified != "" {
		req.Header.Set("If-Modified-Since", prev.LastModified)
	}

	resp, err := client.Do(req)
	if err != nil {
		// Network failure but we have a cached copy — use it.
		// Better to deploy stale config than to fail the
		// reconcile entirely; operator running `vd apply`
		// without internet expected to deploy what they
		// already have.
		if cached, cerr := os.ReadFile(bytesPath); cerr == nil {
			return cached, nil
		}

		return nil, fmt.Errorf("fetch %s: %w", u, err)
	}

	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified {
		cached, cerr := os.ReadFile(bytesPath)
		if cerr != nil {
			return nil, fmt.Errorf("304 from %s but cache missing: %w", u, cerr)
		}

		return cached, nil
	}

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return nil, fmt.Errorf("fetch %s: HTTP %d: %s", u, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}

	if err := os.WriteFile(bytesPath, body, 0644); err != nil {
		// Cache write failure is non-fatal — operator gets
		// the bytes anyway, just no cache for next time.
		h.logf("asset cache: write %s failed: %v", bytesPath, err)
	}

	_ = writeCacheMeta(metaPath, cacheMeta{
		ETag:         resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"),
	})

	return body, nil
}

// cacheMeta is the on-disk JSON sidecar capturing the
// conditional-GET headers the URL source server sent on the
// previous fetch. Empty fields are fine — the next request
// just won't use that header.
type cacheMeta struct {
	ETag         string `json:"etag,omitempty"`
	LastModified string `json:"last_modified,omitempty"`
}

func readCacheMeta(path string) (cacheMeta, error) {
	var m cacheMeta

	raw, err := os.ReadFile(path)
	if err != nil {
		return m, err
	}

	_ = json.Unmarshal(raw, &m)

	return m, nil
}

func writeCacheMeta(path string, m cacheMeta) error {
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}

	return os.WriteFile(path, raw, 0644)
}

// sha256OfString returns the hex sha256 of s. Used as the
// cache key (URL → cache file basename) so the on-disk cache
// is keyed deterministically without depending on URL escape
// quirks.
func sha256OfString(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// atomicWrite writes content to dst by first writing to a
// sibling tempfile and renaming. Avoids partial-write
// corruption when a reconcile crashes mid-write — readers
// either see the old content or the new, never half. Standard
// pattern.
//
// The destination file is forced to mode 0644 (world-readable)
// so containers running as non-root users (redis as UID 999,
// postgres as UID 70, nginx as UID 101 — every official image
// runs as its own service user) can read the bind-mounted
// asset. `os.CreateTemp` defaults to 0600, which would survive
// the rename and break the bind mount with "Permission denied"
// on `:ro` mounts. Assets are NEVER secrets — secrets live in
// /config (env file injected by the controller). World-
// readable on the host is the right posture for the asset
// kind; the docker bridge already filters external access.
func atomicWrite(dst string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(dst), filepath.Base(dst)+".tmp-*")
	if err != nil {
		return err
	}

	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}

	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}

	// CreateTemp lands at 0600; widen so non-root containers
	// can read the bind-mounted file.
	if err := os.Chmod(tmp.Name(), 0644); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}

	return os.Rename(tmp.Name(), dst)
}

// validAssetKey enforces the identifier convention asset keys
// must follow so `${asset.<name>.<key>}` parses unambiguously
// and the key can be a filename on every supported FS.
//
// Allowed: a–z, A–Z, 0–9, underscore, hyphen. Min 1 char.
// Disallowed: dot, slash, whitespace, anything else. Operators
// who'd like a key called `redis.conf` declare it as
// `redis_conf` and supply the in-container filename via the
// mount target (`${asset.X.redis_conf}:/etc/redis/redis.conf`).
func validAssetKey(key string) bool {
	if key == "" {
		return false
	}

	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '_' || r == '-':
		default:
			return false
		}
	}

	return true
}

func (h *AssetHandler) logf(format string, args ...any) {
	if h.Log == nil {
		return
	}

	h.Log.Printf(format, args...)
}
