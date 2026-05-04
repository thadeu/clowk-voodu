// Tests for dockerResources — translation of the wire-shape
// resources block into ContainerSpec's docker-ready (CPULimit,
// MemoryLimitBytes) pair. The k8svalues parsers have their own
// tests covering the grammar surface; this file pins the
// integration:
//
//   - nil / empty wire spec → no limit (caller bypasses --cpus /
//     --memory flag emission)
//   - valid k8s values → docker-format CPU + bytes int64
//   - invalid values bubble up with the original parser error so
//     callers can route them into reconcile-error messages

package controller

import (
	"strings"
	"testing"
)

func TestDockerResources_NilSpecNoLimits(t *testing.T) {
	cpu, mem, err := dockerResources(nil)
	if err != nil {
		t.Fatalf("nil should not error: %v", err)
	}

	if cpu != "" || mem != 0 {
		t.Errorf("nil should yield (\"\", 0), got (%q, %d)", cpu, mem)
	}
}

func TestDockerResources_EmptyLimitsNoLimits(t *testing.T) {
	cpu, mem, err := dockerResources(&resourcesWireSpec{Limits: &resourceLimitsWireSpec{}})
	if err != nil {
		t.Fatal(err)
	}

	if cpu != "" || mem != 0 {
		t.Errorf("empty limits should yield (\"\", 0), got (%q, %d)", cpu, mem)
	}
}

func TestDockerResources_ValidValues(t *testing.T) {
	spec := &resourcesWireSpec{
		Limits: &resourceLimitsWireSpec{
			CPU:    "2",
			Memory: "4Gi",
		},
	}

	cpu, mem, err := dockerResources(spec)
	if err != nil {
		t.Fatal(err)
	}

	if cpu != "2" {
		t.Errorf("cpu: got %q, want 2", cpu)
	}

	if mem != 4*1024*1024*1024 {
		t.Errorf("memory: got %d, want %d", mem, 4*1024*1024*1024)
	}
}

func TestDockerResources_MillicoresNormalised(t *testing.T) {
	spec := &resourcesWireSpec{
		Limits: &resourceLimitsWireSpec{CPU: "500m"},
	}

	cpu, _, err := dockerResources(spec)
	if err != nil {
		t.Fatal(err)
	}

	if cpu != "0.5" {
		t.Errorf("500m should normalise to 0.5, got %q", cpu)
	}
}

func TestDockerResources_DecimalMemory(t *testing.T) {
	spec := &resourcesWireSpec{
		Limits: &resourceLimitsWireSpec{Memory: "1G"},
	}

	_, mem, err := dockerResources(spec)
	if err != nil {
		t.Fatal(err)
	}

	if mem != 1_000_000_000 {
		t.Errorf("1G should be 10^9 bytes, got %d", mem)
	}
}

func TestDockerResources_InvalidCPU(t *testing.T) {
	spec := &resourcesWireSpec{
		Limits: &resourceLimitsWireSpec{CPU: "garbage"},
	}

	_, _, err := dockerResources(spec)
	if err == nil {
		t.Fatal("expected error for garbage cpu")
	}

	if !strings.Contains(err.Error(), "cpu") {
		t.Errorf("error should mention cpu: %v", err)
	}
}

func TestDockerResources_InvalidMemory(t *testing.T) {
	spec := &resourcesWireSpec{
		Limits: &resourceLimitsWireSpec{Memory: "lots"},
	}

	_, _, err := dockerResources(spec)
	if err == nil {
		t.Fatal("expected error for garbage memory")
	}

	if !strings.Contains(err.Error(), "memory") {
		t.Errorf("error should mention memory: %v", err)
	}
}

func TestDockerResources_PartialUnsetOK(t *testing.T) {
	// CPU limit only, no memory cap (or vice versa). Each field
	// is independently optional.
	spec := &resourcesWireSpec{
		Limits: &resourceLimitsWireSpec{CPU: "2"},
	}

	cpu, mem, err := dockerResources(spec)
	if err != nil {
		t.Fatal(err)
	}

	if cpu != "2" {
		t.Errorf("cpu: got %q", cpu)
	}

	if mem != 0 {
		t.Errorf("memory should remain 0, got %d", mem)
	}
}
