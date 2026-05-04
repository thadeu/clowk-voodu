// Resources block on the controller side. The manifest layer
// validates k8s-style values at apply time; the wire shape that
// reaches us here is the same JSON shape, RAW (e.g. {"cpu":"2",
// "memory":"4Gi"}). We re-parse via internal/k8svalues so the
// docker run translation step gets numeric values it can splice
// directly.
//
// Re-parsing on the controller side is defensive: a manifest can
// reach us via paths other than the typed apply (legacy YAML, a
// future plugin emitting JSON directly), so we don't trust the
// upstream validation alone. The cost is negligible — handlers
// run this once per reconcile per resource.

package controller

import (
	"go.voodu.clowk.in/internal/k8svalues"
)

// resourcesWireSpec is the JSON shape the manifest layer
// persists for resources { limits {...} }. Mirrors
// manifest.ResourcesSpec field-for-field with the same tags so
// json.Unmarshal round-trips cleanly. Controller-local because
// internal/manifest imports controller (cycle).
type resourcesWireSpec struct {
	Limits *resourceLimitsWireSpec `json:"limits,omitempty"`
}

type resourceLimitsWireSpec struct {
	CPU    string `json:"cpu,omitempty"`
	Memory string `json:"memory,omitempty"`
}

// dockerResources translates a parsed resources block into the
// (CPULimit string, MemoryLimitBytes int64) pair ContainerSpec
// expects. Empty / nil spec → ("", 0, nil) — caller passes
// through, docker daemon defaults apply (no limit).
//
// Errors here mean an invalid value made it past the manifest
// layer's validate step. Callers (handlers) should surface the
// error so reconcile fails loudly instead of silently emitting
// a container without limits.
func dockerResources(spec *resourcesWireSpec) (cpu string, memBytes int64, err error) {
	if spec == nil || spec.Limits == nil {
		return "", 0, nil
	}

	cpu, err = k8svalues.ParseCPU(spec.Limits.CPU)
	if err != nil {
		return "", 0, err
	}

	memBytes, err = k8svalues.ParseMemoryBytes(spec.Limits.Memory)
	if err != nil {
		return "", 0, err
	}

	return cpu, memBytes, nil
}
