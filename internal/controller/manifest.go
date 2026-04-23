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
// Scope is the diff/prune grouping key. Deployment and ingress require
// a non-empty Scope (parsed from the first HCL label in `deployment
// "scope" "name" { ... }`). All other kinds leave Scope empty — they
// exist at most once per name, so there's nothing for a prune pass to
// group by. Scope is metadata only: it never prefixes container or DNS
// names, and uniqueness of (kind, name) is enforced across scopes so
// two scopes cannot accidentally collide on the same container slot.
type Manifest struct {
	Kind     Kind            `json:"kind"`
	Scope    string          `json:"scope,omitempty"`
	Name     string          `json:"name"`
	Spec     json.RawMessage `json:"spec,omitempty"`
	Metadata *Metadata       `json:"metadata,omitempty"`
}

// ScopedKinds is the set of kinds whose manifests must carry a non-empty
// Scope. Anything else is single-label HCL with no scope concept yet.
var ScopedKinds = map[Kind]bool{
	KindDeployment: true,
	KindIngress:    true,
}

// IsScoped returns true when manifests of kind k must carry a scope.
func IsScoped(k Kind) bool { return ScopedKinds[k] }

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

	if !IsScoped(kind) && m.Scope != "" {
		return fmt.Errorf("%s/%s: scope is not supported for this kind", kind, m.Name)
	}

	return nil
}
