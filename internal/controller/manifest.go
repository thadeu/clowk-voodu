package controller

import (
	"encoding/json"
	"fmt"
	"time"
)

// Manifest is the on-the-wire shape for a declared resource. In M3 Spec
// is arbitrary JSON — the HCL schema comes in M4 and will translate into
// this same shape before writing to etcd, so the store never learns HCL.
type Manifest struct {
	Kind     Kind            `json:"kind"`
	Name     string          `json:"name"`
	Spec     json.RawMessage `json:"spec,omitempty"`
	Metadata *Metadata       `json:"metadata,omitempty"`
}

// Metadata is controller-managed bookkeeping written alongside the spec.
// Callers may leave it nil on apply; the store fills it in.
type Metadata struct {
	CreatedAt time.Time `json:"created_at,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
	Revision  int64     `json:"revision,omitempty"`
}

// Validate checks the minimum viable shape. Per-kind schema validation
// (e.g. deployment must have an image) happens when the M4 HCL parser
// translates to Manifest — this is only guarding against junk writes.
func (m *Manifest) Validate() error {
	if m.Name == "" {
		return fmt.Errorf("manifest name is required")
	}

	if _, err := ParseKind(string(m.Kind)); err != nil {
		return err
	}

	return nil
}
