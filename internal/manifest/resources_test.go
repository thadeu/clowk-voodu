// Tests for ValidateResources — the apply-time guard that runs
// the k8svalues parsers and folds the result into a manifest-
// path-prefixed error. The k8svalues package has its own tests
// covering the parser surface in isolation; this file pins the
// integration: invalid values surface with a "resources.limits.X"
// path so operators can locate the offending field immediately.

package manifest

import (
	"strings"
	"testing"
)

func TestValidateResources_NilAndEmptyOK(t *testing.T) {
	if err := ValidateResources(nil); err != nil {
		t.Errorf("nil should validate: %v", err)
	}

	if err := ValidateResources(&ResourcesSpec{}); err != nil {
		t.Errorf("empty resources block should validate: %v", err)
	}

	if err := ValidateResources(&ResourcesSpec{Limits: &ResourceLimits{}}); err != nil {
		t.Errorf("empty limits block should validate: %v", err)
	}
}

func TestValidateResources_GoodValues(t *testing.T) {
	spec := &ResourcesSpec{
		Limits: &ResourceLimits{
			CPU:    "2",
			Memory: "4Gi",
		},
	}

	if err := ValidateResources(spec); err != nil {
		t.Errorf("valid spec rejected: %v", err)
	}
}

func TestValidateResources_BadCPU(t *testing.T) {
	spec := &ResourcesSpec{
		Limits: &ResourceLimits{CPU: "invalid", Memory: "4Gi"},
	}

	err := ValidateResources(spec)
	if err == nil {
		t.Fatal("expected error")
	}

	if !strings.Contains(err.Error(), "resources.limits.cpu") {
		t.Errorf("error should mention path: %v", err)
	}
}

func TestValidateResources_BadMemory(t *testing.T) {
	spec := &ResourcesSpec{
		Limits: &ResourceLimits{CPU: "2", Memory: "garbage"},
	}

	err := ValidateResources(spec)
	if err == nil {
		t.Fatal("expected error")
	}

	if !strings.Contains(err.Error(), "resources.limits.memory") {
		t.Errorf("error should mention path: %v", err)
	}
}
