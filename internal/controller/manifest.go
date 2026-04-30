package controller

import (
	"encoding/json"
	"fmt"
	"time"
)

// Manifest is the on-the-wire shape for a declared resource. Spec is
// arbitrary JSON; the parser in internal/manifest decodes HCL/YAML into
// this shape before the controller ever sees it.
//
// Scope is both a diff/prune grouping key AND part of the app identity
// on disk and in docker. Deployment and ingress require a non-empty
// Scope (parsed from the first HCL label in `deployment "scope" "name"
// { ... }`). Uniqueness of (kind, name) is enforced per-scope only:
// two scopes may both declare `deployment "web"` because their
// container slots, image tags, release dirs and env files are keyed by
// AppID(scope, name), which collapses to `<scope>-<name>`. Other kinds
// leave Scope empty — they exist at most once per name.
type Manifest struct {
	Kind     Kind            `json:"kind"`
	Scope    string          `json:"scope,omitempty"`
	Name     string          `json:"name"`
	Spec     json.RawMessage `json:"spec,omitempty"`
	Metadata *Metadata       `json:"metadata,omitempty"`
}

// AppID is the canonical on-host identifier for a (scope, name) pair.
// Every filesystem path, container name, image tag, and env-file the
// platform materialises for a deployment is keyed by this string so
// multiple scopes can reuse the same name without colliding.
//
//	AppID("prod", "web")  == "prod-web"
//	AppID("", "postgres") == "postgres"        // unscoped kinds
//
// Unscoped callers (databases, generic services) pass scope == "" and
// get the bare name back. Deployment and ingress — the scoped kinds —
// always carry a scope, so the "-" form is the effective shape in
// practice. Keep this function the single source of truth for the
// derivation; ad-hoc `scope + "-" + name` concatenations will drift.
func AppID(scope, name string) string {
	if scope == "" {
		return name
	}

	return scope + "-" + name
}

// ScopedKinds is the set of kinds whose manifests MUST carry a
// non-empty Scope. Asset is intentionally absent — it's the only
// kind that's optionally scoped (1-label = unscoped global,
// 2-label = scoped). Other kinds are strictly 2-label.
var ScopedKinds = map[Kind]bool{
	KindDeployment:  true,
	KindStatefulset: true,
	KindIngress:     true,
	KindJob:         true,
	KindCronJob:     true,
}

// OptionallyScopedKinds is the set of kinds whose manifests
// MAY carry a scope. Today only asset — operators declare
// `asset "name" { … }` for global / shared bundles or
// `asset "scope" "name" { … }` for scope-local ones. The
// reference syntax distinguishes them: 3-segment refs resolve
// unscoped, 4-segment refs resolve scoped.
var OptionallyScopedKinds = map[Kind]bool{
	KindAsset: true,
}

// IsScoped returns true when manifests of kind k must carry a scope.
func IsScoped(k Kind) bool { return ScopedKinds[k] }

// IsOptionallyScoped reports whether kind k accepts both
// scoped and unscoped manifests. Today only asset.
func IsOptionallyScoped(k Kind) bool { return OptionallyScopedKinds[k] }

// IsCoreKind reports whether k is one of the controller's built-in
// kinds (the ones with hand-written reconcile handlers). Anything
// else is presumed to be a plugin-block kind whose lifecycle is
// macro-expanded by an installed plugin into one or more core
// kinds before persistence. Used by /apply to route between the
// direct-store path and the plugin-expand path.
func IsCoreKind(k Kind) bool {
	return validKinds[k]
}

// Metadata is controller-managed bookkeeping written alongside the spec.
// Callers may leave it nil on apply; the store fills it in.
type Metadata struct {
	CreatedAt time.Time `json:"created_at,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
	Revision  int64     `json:"revision,omitempty"`
}

// Validate checks the minimum viable shape. Per-kind schema validation
// (e.g. deployment must have an image) happens when the HCL parser
// translates to Manifest — this is only guarding against junk writes.
func (m *Manifest) Validate() error {
	if m.Name == "" {
		return fmt.Errorf("manifest name is required")
	}

	kind, err := ParseKind(string(m.Kind))
	if err != nil {
		return err
	}

	if IsScoped(kind) && m.Scope == "" {
		return fmt.Errorf("%s/%s: scope is required (use `%s \"scope\" \"%s\" { ... }`)", kind, m.Name, kind, m.Name)
	}

	// Optionally-scoped kinds (asset) accept both empty
	// and non-empty scope — both are valid manifests with
	// distinct semantics (unscoped global vs scope-local).
	if !IsScoped(kind) && !IsOptionallyScoped(kind) && m.Scope != "" {
		return fmt.Errorf("%s/%s: scope is not supported for this kind", kind, m.Name)
	}

	return nil
}
