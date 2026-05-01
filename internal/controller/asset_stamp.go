package controller

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

// consumerKinds is the set of kinds whose specs may carry asset
// references — either as textual `${asset.…}` placeholders inside
// string fields (volumes, command, env values, image, ports) or
// as explicit entries under `depends_on.assets`. Stamping iterates
// this set when scanning a batch for consumers to enrich.
var consumerKinds = map[Kind]bool{
	KindDeployment:  true,
	KindStatefulset: true,
	KindJob:         true,
	KindCronJob:     true,
}

// IsConsumerKind reports whether kind k may consume assets.
// Used by the stamping pipeline to skip non-consumer manifests
// (assets themselves, ingress, service, etc) without touching them.
func IsConsumerKind(k Kind) bool { return consumerKinds[k] }

// StampAssetDigests resolves every asset reference in `manifests`
// (textual ${asset.…} + explicit depends_on.assets) and embeds
// an `_asset_digests` map into each consumer manifest's spec.
//
// Why this exists: the consumer hash function folds asset digests
// into the spec hash so asset content drift triggers rolling
// restart. Pre-stamping, the hash function read digests from
// /status at consumer reconcile time — but consumer reconciles
// often raced ahead of the asset reconcile, reading STALE digests
// and missing drift. Stamping moves digest-binding from runtime
// (the /status side-channel) to apply time (the consumer's
// /desired blob carries its own digests), so the /desired blob
// itself diff's whenever the asset bytes change. The watch fires,
// the consumer recomputes its hash, and rolling restart flows
// through the existing reconcile machinery — no fan-out from
// asset → consumer needed.
//
// Pipeline (post plugin-expand, pre /desired persist):
//
//  1. Build batchDigests: sha256 every asset's bytes in this
//     apply. Sources resolved exactly as in AssetHandler.apply
//     (file + inline are local; url() goes through the shared
//     cache so the round-trip primes both phases).
//  2. For each consumer in `manifests`: collect refs from spec
//     strings + depends_on.assets, look up each ref's digest
//     (batchDigests first, /status fallback for cross-batch),
//     stamp the result under spec._asset_digests.
//
// Failure modes:
//
//   - asset source decode failure (bad base64 in file, malformed
//     url object): logged and the key is skipped — consumers
//     referencing that key surface an unresolved-ref error in
//     phase 2, so the apply still rejects loudly.
//   - url() unreachable but /status has a previous digest: stamp
//     the stale digest, log a warning, apply continues. Mirrors
//     AssetHandler.apply's stale-success posture.
//   - url() unreachable AND no /status fallback: digest absent;
//     consumer phase rejects the apply with the formatted ref.
//   - consumer references an asset that doesn't exist anywhere
//     (typo, undeclared): apply rejected with the formatted ref
//     in the same shape the operator typed (3-seg or 4-seg).
func StampAssetDigests(
	ctx context.Context,
	store Store,
	httpClient *http.Client,
	logger *log.Logger,
	manifests []*Manifest,
) error {
	if len(manifests) == 0 {
		return nil
	}

	logf := func(format string, args ...any) {
		if logger != nil {
			logger.Printf(format, args...)
		}
	}

	// Phase 1: materialise every asset in the batch up front.
	// Single pass writes bytes to disk, computes sha256, and
	// updates /status. Critical for race-avoidance: when the
	// /desired Put fires watches downstream, consumer reconciles
	// can race ahead of the asset reconcile and bind-mount a
	// non-existent source path — docker creates that path as a
	// directory, then atomicWrite from the asset reconcile
	// errors EISDIR and the container is stuck with an empty
	// dir inside forever.
	//
	// Materialising inline (here, before /desired persist)
	// guarantees the bytes are on disk by the time any watch
	// fires, so consumer reconciles always see real files at
	// the bind-mount sources.
	batchDigests := make(map[assetRef]string)

	for _, m := range manifests {
		if m == nil || m.Kind != KindAsset {
			continue
		}

		digests, _, err := materializeAssetSpec(ctx, store, httpClient, logf, m)
		if err != nil {
			return fmt.Errorf("asset/%s/%s: %w", m.Scope, m.Name, err)
		}

		for key, sum := range digests {
			batchDigests[assetRef{Scope: m.Scope, Name: m.Name, Key: key}] = sum
		}
	}

	// Phase 2: stamp each consumer with the digests of refs it
	// actually uses. Consumers without refs are left untouched
	// (no _asset_digests field added) so the spec stays minimal.
	for _, m := range manifests {
		if m == nil || !IsConsumerKind(m.Kind) {
			continue
		}

		if err := stampConsumerSpec(ctx, store, m, batchDigests); err != nil {
			return fmt.Errorf("%s/%s/%s: %w", m.Kind, m.Scope, m.Name, err)
		}
	}

	return nil
}

// On-failure modes for url() sources. Mirror what the HCL
// `url("…", { on_failure = "…" })` option accepts.
const (
	OnFailureError = "error" // apply rejects on fetch failure
	OnFailureStale = "stale" // fall back to last-known-good (default)
	OnFailureSkip  = "skip"  // omit key; consumer ref is best-effort
)

// sourceOptions are the operator-supplied knobs that ride
// alongside a source object. Only `url()` populates these
// today — file/inline sources have no failure mode (they
// either decode or don't).
type sourceOptions struct {
	OnFailure string // OnFailureError | OnFailureStale | OnFailureSkip
}

// resolveAssetSourceForStamping mirrors AssetHandler.resolveSource
// but is a free function callable without an AssetHandler
// instance. Behaviour matches byte-for-byte: inline string →
// bytes verbatim; {_source:file,content:b64} → base64-decoded;
// {_source:url,url:...,timeout?,on_failure?} → fetched via the
// shared ETag cache with the operator-supplied timeout.
//
// Returns the source options alongside the bytes so the caller
// (materializeAssetSpec) can apply on_failure semantics when
// the fetch fails. Non-url sources return an empty sourceOptions.
func resolveAssetSourceForStamping(ctx context.Context, client *http.Client, raw json.RawMessage) ([]byte, sourceOptions, error) {
	trimmed := strings.TrimSpace(string(raw))

	if len(trimmed) == 0 {
		return nil, sourceOptions{}, fmt.Errorf("empty source")
	}

	if trimmed[0] == '"' {
		var s string

		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, sourceOptions{}, fmt.Errorf("decode inline string: %w", err)
		}

		return []byte(s), sourceOptions{}, nil
	}

	var obj map[string]any

	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, sourceOptions{}, fmt.Errorf("decode source object: %w", err)
	}

	src, _ := obj["_source"].(string)

	switch src {
	case "file":
		content, ok := obj["content"].(string)
		if !ok {
			return nil, sourceOptions{}, fmt.Errorf(`file source missing "content" string`)
		}

		decoded, err := base64.StdEncoding.DecodeString(content)
		if err != nil {
			return nil, sourceOptions{}, fmt.Errorf("file source: invalid base64: %w", err)
		}

		return decoded, sourceOptions{}, nil

	case "url":
		u, ok := obj["url"].(string)
		if !ok {
			return nil, sourceOptions{}, fmt.Errorf(`url source missing "url" string`)
		}

		timeout := parseDurationOrZero(obj["timeout"])
		opts := sourceOptions{OnFailure: stringOr(obj["on_failure"], OnFailureStale)}

		bytes, err := fetchAssetURLShared(ctx, client, nil, u, timeout)
		if err != nil {
			return nil, opts, err
		}

		return bytes, opts, nil

	default:
		return nil, sourceOptions{}, fmt.Errorf("unknown asset source %q (want file|url|inline)", src)
	}
}

// parseDurationOrZero turns a JSON-decoded value into a
// time.Duration. Accepts a Go-style duration string ("5s",
// "2m"); empty / nil / wrong type / unparseable → 0, which
// fetchAssetURLShared maps to the default timeout. Garbage
// strings don't error here because the parser already
// validated the shape; downstream is permissive on purpose.
func parseDurationOrZero(v any) time.Duration {
	s, ok := v.(string)
	if !ok || s == "" {
		return 0
	}

	d, err := time.ParseDuration(s)
	if err != nil {
		return 0
	}

	return d
}

// stringOr returns v as a string if it's a non-empty one;
// otherwise the fallback. Used to apply defaults to operator-
// supplied options without writing the same conditional
// inline at every call site.
func stringOr(v any, fallback string) string {
	s, ok := v.(string)
	if !ok || s == "" {
		return fallback
	}

	return s
}

// loadAssetStatus reads /status/assets/<scope>-<name> and decodes
// AssetStatus. Returns nil on any failure (status missing,
// decode error) — caller treats nil as "no prior digest" and
// surfaces the unresolved ref in the apply error.
func loadAssetStatus(ctx context.Context, store Store, scope, name string) *AssetStatus {
	if store == nil {
		return nil
	}

	raw, err := store.GetStatus(ctx, KindAsset, AppID(scope, name))
	if err != nil || raw == nil {
		return nil
	}

	var st AssetStatus

	if err := json.Unmarshal(raw, &st); err != nil {
		return nil
	}

	return &st
}

// stampConsumerSpec mutates m.Spec in place: walks the spec for
// asset refs (textual + depends_on.assets), resolves each via
// batchDigests then /status, and writes the result under
// `_asset_digests`. Returns an error when any ref is unresolvable
// (asset doesn't exist in the batch or in /status).
//
// Idempotent: re-stamping a consumer that already has
// _asset_digests overwrites the field with the freshly-resolved
// digests. Stale entries from a previous apply don't leak.
func stampConsumerSpec(ctx context.Context, store Store, m *Manifest, batchDigests map[assetRef]string) error {
	if len(m.Spec) == 0 {
		return nil
	}

	var spec map[string]any

	if err := json.Unmarshal(m.Spec, &spec); err != nil {
		return fmt.Errorf("decode spec: %w", err)
	}

	// Drop any stamped digests from a previous apply round so
	// the re-stamp is a clean overwrite, not a merge. Dropping
	// happens before ref collection so the walker doesn't
	// pick up old references via the digest map keys.
	delete(spec, "_asset_digests")

	refs := collectRefsFromSpecMap(spec)

	// Explicit depends_on.assets entries — the operator-declared
	// dependencies that may not be visible as textual refs.
	if dep, ok := spec["depends_on"].(map[string]any); ok {
		if assets, ok := dep["assets"].([]any); ok {
			for _, a := range assets {
				str, ok := a.(string)
				if !ok {
					return fmt.Errorf("depends_on.assets[*]: expected string, got %T", a)
				}

				ref, err := parseDependsOnRef(str)
				if err != nil {
					return fmt.Errorf("depends_on.assets[%q]: %w", str, err)
				}

				refs = append(refs, ref)
			}
		}
	}

	if len(refs) == 0 {
		// No deps → nothing to stamp. Re-marshal anyway because
		// we may have dropped a stale _asset_digests above.
		bytes, err := json.Marshal(spec)
		if err != nil {
			return err
		}

		m.Spec = bytes

		return nil
	}

	// Resolve each ref. Dedupe via the formatted-ref string —
	// the same (scope, name, key) tuple typed twice in volumes
	// stamps once.
	digests := make(map[string]string, len(refs))

	for _, ref := range refs {
		formatted := formatAssetRef(ref.Scope, ref.Name, ref.Key)

		if _, already := digests[formatted]; already {
			continue
		}

		if d, ok := batchDigests[ref]; ok {
			digests[formatted] = d
			continue
		}

		// Cross-batch ref: asset isn't in this apply but may
		// exist in /status from a prior reconcile. Common case
		// for `vd apply -f infra/redis` after the asset was
		// applied earlier from a different file.
		if st := loadAssetStatus(ctx, store, ref.Scope, ref.Name); st != nil {
			if d, ok := st.Files[ref.Key]; ok && d != "" {
				digests[formatted] = d
				continue
			}
		}

		return fmt.Errorf("unresolved asset reference %q (asset not in this apply, no prior /status — typo, or apply the asset first)", formatted)
	}

	spec["_asset_digests"] = digests

	bytes, err := json.Marshal(spec)
	if err != nil {
		return err
	}

	m.Spec = bytes

	return nil
}

// collectRefsFromSpecMap recurses through the un-typed unmarshal
// of a spec JSON blob and extracts every `${asset.…}` ref it
// finds in string values. Skips:
//
//   - keys prefixed with "_" (controller-internal — never
//     authored, never scanned)
//   - the "depends_on" key (handled explicitly by caller via
//     parseDependsOnRef so the format error path stays distinct
//     from accidental textual matches inside the depends_on
//     block)
//
// Walking generically (not per-kind schema) works for every
// current consumer kind and any future one — as long as the
// kind's spec is a JSON object whose string-typed leaves can
// carry refs.
func collectRefsFromSpecMap(spec map[string]any) []assetRef {
	var out []assetRef

	walkSpecForRefs(spec, &out)

	return out
}

func walkSpecForRefs(v any, out *[]assetRef) {
	switch x := v.(type) {
	case string:
		for _, m := range assetRefPattern.FindAllStringSubmatch(x, -1) {
			*out = append(*out, assetRef{Scope: m[1], Name: m[2], Key: m[3]})
		}

	case []any:
		for _, e := range x {
			walkSpecForRefs(e, out)
		}

	case map[string]any:
		for k, e := range x {
			if strings.HasPrefix(k, "_") || k == "depends_on" {
				continue
			}

			walkSpecForRefs(e, out)
		}
	}
}

// parseDependsOnRef parses a "scope.name.key" or "name.key"
// string into an assetRef. Mirror of formatAssetRef — same
// 2-vs-3-segment routing.
func parseDependsOnRef(s string) (assetRef, error) {
	parts := strings.Split(s, ".")

	switch len(parts) {
	case 2:
		if parts[0] == "" || parts[1] == "" {
			return assetRef{}, fmt.Errorf("invalid 2-segment ref (empty segment)")
		}

		return assetRef{Name: parts[0], Key: parts[1]}, nil

	case 3:
		if parts[0] == "" || parts[1] == "" || parts[2] == "" {
			return assetRef{}, fmt.Errorf("invalid 3-segment ref (empty segment)")
		}

		return assetRef{Scope: parts[0], Name: parts[1], Key: parts[2]}, nil

	default:
		return assetRef{}, fmt.Errorf("expected 2 or 3 segments separated by '.', got %d", len(parts))
	}
}
