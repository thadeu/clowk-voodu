package controller

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
)

// RegistryHandler reconciles `registry "name" { url, username, token }`
// manifests by atomically regenerating the host's docker config.json
// so subsequent `docker pull` calls authenticate against private
// registries transparently — no manual `docker login` on the box.
//
// Voodu owns the docker config file ENTIRELY. The handler does NOT
// merge with any pre-existing manual logins. The rationale is
// simplicity: a voodu-managed host's authoritative source of truth
// for pull-secrets is the manifest store, so reconciling means
// "what /desired/registrys/* says, exactly that, nothing more".
// Operators who used `docker login` manually before adopting voodu
// will see those entries overwritten on the first apply containing
// a registry block (or on the first delete of one). Document this
// loudly in user-facing release notes — the alternative (preserve-
// unknown entries) introduces "where did this auth come from?"
// mysteries that the simplicity win isn't worth.
//
// Atomicity is non-negotiable: docker reads config.json on every
// pull, and a partially-written file would race with a deployment's
// startup pull and produce flaky "unauthorized" failures. The
// handler writes to a sibling `.tmp` then `os.Rename`s into place —
// rename is atomic on POSIX, so any concurrent reader sees either
// the full old file or the full new file, never a partial mix.
//
// Identity model: each manifest is keyed by name (the HCL block
// label), but the resulting docker config entry is keyed by URL.
// Two manifests pointing at the same URL would collide silently
// (last writer in the iteration order wins); the parse layer's
// duplicate-identity check guards against same-NAME collisions
// but URL collisions are a manifest-design footgun the handler
// surfaces only as a log line ("multiple registries claim X —
// using Y"). In practice operators name the manifest after the
// URL (`registry "ghcr"` for ghcr.io, `registry "dockerhub"`
// for index.docker.io), so the collision case is exotic.
type RegistryHandler struct {
	Store Store
	Log   *log.Logger

	// DockerConfigPath is the absolute path of the docker client's
	// auth config file. Production: `~/.docker/config.json` (resolved
	// from $HOME at server-init time so the controller-process user
	// — usually `voodu` or `root` — gets the file IT will use when
	// shelling out to `docker pull`). Tests inject a tempdir path
	// to avoid mutating the real config on the dev machine.
	//
	// Empty falls back to the runtime default — see ensureConfigPath.
	// This shape (field + lazy default) lets the handler stay test-
	// injectable without forcing every production caller to wire
	// HOME resolution themselves.
	DockerConfigPath string
}

// dockerConfig is the wire shape of `~/.docker/config.json`. Only
// the fields we touch are modelled explicitly; we use a generic
// map for the rest so an existing config with unrelated keys
// (HTTPHeaders, credsStore, plugins, etc.) round-trips losslessly
// when other tools have written it.
//
// In the current "voodu owns config.json" stance we still preserve
// unknown top-level keys verbatim — the cost is one map field, and
// the upside is that voodu coexists with rare manual customisations
// (e.g. an operator setting `credsStore = "osxkeychain"` for an
// orthogonal set of credentials) without clobbering them. Only the
// `auths` section is treated as fully-owned and rewritten from
// scratch each reconcile.
type dockerConfig struct {
	Auths map[string]dockerAuth `json:"auths"`

	// Extra captures every other top-level key in the file so the
	// reconciler can round-trip them without knowing their shape.
	// Populated by load(); emitted by save() interleaved with the
	// Auths key.
	Extra map[string]json.RawMessage `json:"-"`
}

// dockerAuth is one entry in the `auths` map. The `auth` field is
// the literal base64 encoding of `username:token` — what docker's
// CLI writes after a successful `docker login`. Other fields
// (email, identitytoken) are intentionally omitted: they're
// optional in docker's schema, and adding them is busywork that
// doesn't authenticate any registry we know of in 2026.
type dockerAuth struct {
	Auth string `json:"auth"`
}

// Handle dispatches per WatchEvent type. Mirrors every other
// reconciler handler — small, predictable. Both Put and Delete
// regenerate config.json from scratch by listing every registry
// manifest in the store, so the two paths share the same
// "rewrite, don't patch" semantic.
func (h *RegistryHandler) Handle(ctx context.Context, ev WatchEvent) error {
	switch ev.Type {
	case WatchPut, WatchDelete:
		return h.regenerate(ctx, ev)
	}

	return nil
}

// regenerate is the single code path both Put and Delete take.
// Listing every registry in /desired produces the complete
// authoritative `auths` map; the on-disk file is then rewritten
// atomically. Race-with-other-applies safety: two concurrent
// applies of different registry blocks each call regenerate(),
// each lists from the store AFTER its own write has settled
// (etcd Put is linearisable), so the later-writing reconcile
// sees the earlier one's effect. The on-disk file ends up
// reflecting whichever apply landed second — which is what
// "eventually consistent reconcile" looks like.
func (h *RegistryHandler) regenerate(ctx context.Context, ev WatchEvent) error {
	all, err := h.Store.List(ctx, KindRegistry)
	if err != nil {
		return fmt.Errorf("list registries: %w", err)
	}

	cfg, err := h.loadOrInit()
	if err != nil {
		return fmt.Errorf("load docker config: %w", err)
	}

	// Voodu-owned section: rebuild from scratch. Any auth entry
	// not declared in a manifest disappears here — that's the
	// "voodu owns config.json entirely" stance the type's
	// godoc documents.
	cfg.Auths = make(map[string]dockerAuth, len(all))

	for _, m := range all {
		if m == nil {
			continue
		}

		spec, derr := decodeRegistrySpec(m)
		if derr != nil {
			// Surface as a log line and skip — one malformed
			// manifest shouldn't take down auth for every
			// other registry. The reconciler will retry the
			// bad manifest on its next watch event.
			h.logf("registry/%s: decode spec: %v (skipping in auth file)", m.Name, derr)
			continue
		}

		if existing, dupe := cfg.Auths[spec.URL]; dupe {
			h.logf("registry/%s: URL %q already claimed (auth=%q…); overwriting with this manifest's credentials",
				m.Name, spec.URL, truncate(existing.Auth, 8))
		}

		cfg.Auths[spec.URL] = dockerAuth{
			Auth: encodeBasicAuth(spec.Username, spec.Token),
		}
	}

	if err := h.save(cfg); err != nil {
		return fmt.Errorf("write docker config: %w", err)
	}

	h.logf("registry: regenerated %s with %d auth(s) (event=%s name=%s)",
		h.ensureConfigPath(), len(cfg.Auths), ev.Type, ev.Name)

	return nil
}

// registrySpec mirrors manifest.RegistrySpec — the controller
// re-decodes the wire JSON. Defined locally to avoid the reverse
// import (manifest already imports controller). Keep field
// names/JSON tags in lock-step with the manifest type.
type registrySpec struct {
	URL      string `json:"url"`
	Username string `json:"username"`
	Token    string `json:"token"`
}

func decodeRegistrySpec(m *Manifest) (registrySpec, error) {
	var s registrySpec

	if m == nil {
		return s, fmt.Errorf("nil manifest")
	}

	if len(m.Spec) == 0 {
		return s, fmt.Errorf("empty spec")
	}

	if err := json.Unmarshal(m.Spec, &s); err != nil {
		return s, err
	}

	if s.URL == "" || s.Username == "" || s.Token == "" {
		return s, fmt.Errorf("registry spec missing required field(s) (url=%q username=%q token=%t)",
			s.URL, s.Username, s.Token != "")
	}

	return s, nil
}

// encodeBasicAuth is the docker config convention for the `auth`
// field: literal base64(`<username>:<token>`), no `Basic` prefix
// (the prefix is added at HTTP wire time when docker reads this
// file and constructs the Authorization header). StdEncoding,
// no padding stripping — matches what `docker login` produces.
func encodeBasicAuth(username, token string) string {
	return base64.StdEncoding.EncodeToString([]byte(username + ":" + token))
}

// loadOrInit reads the on-disk config (if any), tolerating both
// missing-file (first-ever reconcile on this host) and malformed-
// JSON (operator hand-edited or partial-write from some other
// tool) by returning a fresh empty config in either case.
//
// Round-tripping unknown top-level keys: we unmarshal into a flat
// map first to capture every key, then split out the `auths`
// section into the typed sub-struct. Other keys land in cfg.Extra
// for save() to re-emit. The split keeps `auths` strongly typed
// (we mutate it heavily) while preserving the rest verbatim.
func (h *RegistryHandler) loadOrInit() (dockerConfig, error) {
	path := h.ensureConfigPath()

	raw, err := os.ReadFile(path)
	if err != nil {
		// First reconcile (no file) and malformed-JSON paths
		// both converge here: a fresh empty config that will
		// get populated on the way out. The save() call below
		// MkdirAll's the parent on its own, so a missing
		// ~/.docker/ on first apply is fine.
		if os.IsNotExist(err) {
			return dockerConfig{Auths: map[string]dockerAuth{}, Extra: map[string]json.RawMessage{}}, nil
		}

		return dockerConfig{}, err
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		// Malformed JSON — log loudly and start fresh rather
		// than wedge the reconcile loop forever. Operators
		// who hand-edited the file see the warning in
		// `journalctl -u voodu-controller` and either fix
		// the file or accept the rewrite.
		h.logf("registry: existing %s is unparseable (%v); rewriting from scratch", path, err)

		return dockerConfig{Auths: map[string]dockerAuth{}, Extra: map[string]json.RawMessage{}}, nil
	}

	cfg := dockerConfig{
		Auths: map[string]dockerAuth{},
		Extra: make(map[string]json.RawMessage, len(top)),
	}

	for k, v := range top {
		if k == "auths" {
			// Best-effort decode: a malformed auths block
			// gets wiped and rebuilt rather than blocking
			// the rest of the file. The voodu-owns-it stance
			// makes this safe — we'd overwrite it anyway.
			_ = json.Unmarshal(v, &cfg.Auths)
			continue
		}

		cfg.Extra[k] = v
	}

	return cfg, nil
}

// save writes the config to its `.tmp` sibling and os.Renames
// into place. Rename is atomic on POSIX (the file system
// transitions in one step from "name points at old inode" to
// "name points at new inode"), so any concurrent docker process
// reading config.json sees either the full old contents or the
// full new — never a half-flushed mix.
//
// Mode 0600: docker's own `docker login` lands the file at 0600
// (it contains base64'd credentials), and any wider perm would
// leak the auths to other local users via the world-readable
// file. Match docker's posture here exactly.
func (h *RegistryHandler) save(cfg dockerConfig) error {
	path := h.ensureConfigPath()

	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("ensure parent dir: %w", err)
	}

	// Merge auths + extras back into a flat map so the on-disk
	// shape matches what docker (and any other tools that wrote
	// the file) expect: top-level keys are an arbitrary
	// mix, not a strict struct.
	out := make(map[string]json.RawMessage, len(cfg.Extra)+1)

	for k, v := range cfg.Extra {
		out[k] = v
	}

	// Always emit `auths`, even when empty, so a deleted-the-last-
	// registry case leaves a coherent {"auths":{}} file behind
	// instead of an absent key (docker tolerates both, but the
	// explicit shape is friendlier to anyone `cat`ing the file
	// during incident response).
	authsBlob, err := json.Marshal(cfg.Auths)
	if err != nil {
		return fmt.Errorf("marshal auths: %w", err)
	}

	out["auths"] = authsBlob

	body, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}

	tmpName := tmp.Name()

	// Ensure the temp file is cleaned up on any error path
	// before the rename succeeds — leaving stray `.tmp-XXXX`
	// files in `~/.docker/` would accumulate and confuse
	// `ls` output over time.
	defer func() {
		if _, statErr := os.Stat(tmpName); statErr == nil {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp: %w", err)
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}

	// 0600 matches what `docker login` writes natively. Tight
	// perm because the file holds base64 creds.
	if err := os.Chmod(tmpName, 0600); err != nil {
		return fmt.Errorf("chmod temp: %w", err)
	}

	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename: %w", err)
	}

	return nil
}

// ensureConfigPath resolves the on-disk destination. Tests inject
// DockerConfigPath directly; production leaves it empty and we
// derive `$HOME/.docker/config.json` on demand. Lazy resolution
// means a controller that never sees a registry block doesn't
// touch HOME-resolution at all (some embedded / unit-test runs
// don't have HOME set, and we don't want them to fail with a
// confusing "registry handler requires HOME" until they actually
// reconcile a registry).
func (h *RegistryHandler) ensureConfigPath() string {
	if h.DockerConfigPath != "" {
		return h.DockerConfigPath
	}

	home := os.Getenv("HOME")
	if home == "" {
		// Last-ditch fallback so the reconcile doesn't NPE on
		// a HOME-less environment. The path won't be writable
		// in most realistic deployments, but the save() error
		// will be clear about what went wrong.
		home = "/root"
	}

	return filepath.Join(home, ".docker", "config.json")
}

func (h *RegistryHandler) logf(format string, args ...any) {
	if h.Log == nil {
		return
	}

	h.Log.Printf(format, args...)
}

// truncate trims s to at most n characters for log output. Used
// when reporting overwritten auth entries — the full base64 is
// secret-equivalent, so we never log more than a prefix.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}

	return s[:n]
}
