// Apply-time validation for the resources { limits { ... } }
// HCL block. The actual k8s-value parsing logic lives in
// internal/k8svalues so the controller can reuse it without
// dragging in the whole manifest package (and the dep cycle that
// would close).

package manifest

import (
	"fmt"

	"go.voodu.clowk.in/internal/k8svalues"
)

// ValidateResources runs apply-time checks on a parsed Resources
// block. Returns nil for empty / unset values (no constraint =
// no limit, perfectly valid). Errors include the offending value
// + format hint so the operator can fix immediately.
func ValidateResources(spec *ResourcesSpec) error {
	if spec == nil || spec.Limits == nil {
		return nil
	}

	if _, err := k8svalues.ParseCPU(spec.Limits.CPU); err != nil {
		return fmt.Errorf("resources.limits.cpu: %w", err)
	}

	if _, err := k8svalues.ParseMemoryBytes(spec.Limits.Memory); err != nil {
		return fmt.Errorf("resources.limits.memory: %w", err)
	}

	return nil
}
