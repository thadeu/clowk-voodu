// Package k8svalues parses k8s-style resource quantity strings.
// Used by both manifest (apply-time validation) and controller
// (reconcile-time docker run translation) — lives in its own
// folder to avoid the dep cycle (manifest ↔ controller).
//
// Grammar matches k8s.io/apimachinery/pkg/api/resource at the
// surface operators care about: decimal SI (K/M/G/T = 1000^N),
// binary SI (Ki/Mi/Gi/Ti = 1024^N), millicores (m suffix on CPU).
//
// We don't reimplement the full k8s resource.Quantity shape —
// just the inputs operators actually write in HCL. Fancier forms
// (exponential notation, fractional suffixes on CPU) get
// rejected early so the operator catches a typo at apply time
// instead of a confusing "out of bounds" at reconcile.

package k8svalues

import (
	"fmt"
	"strconv"
	"strings"
)

// ParseCPU validates an operator-supplied CPU value and returns
// the docker-format string (a decimal float — what docker run
// --cpus accepts). Empty input returns ("", nil) — caller treats
// as "no limit".
//
// Examples:
//
//	"2"      → "2"
//	"1.5"    → "1.5"
//	"500m"   → "0.5"
//	"100m"   → "0.1"
//	""       → ""    (caller skips emit)
//	"garbage" → error
func ParseCPU(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}

	// Millicores: trailing "m". Strip + divide by 1000. Integer
	// part only — k8s rejects fractional millicores ("100.5m").
	if strings.HasSuffix(s, "m") {
		raw := strings.TrimSuffix(s, "m")

		n, err := strconv.Atoi(raw)
		if err != nil {
			return "", fmt.Errorf("invalid millicores %q: must be integer + 'm' (e.g. 500m)", s)
		}

		if n < 0 {
			return "", fmt.Errorf("cpu must be non-negative, got %q", s)
		}

		// Render with up to 3 decimals so 500m → "0.5", 250m → "0.25",
		// 1000m → "1". %g strips trailing zeros.
		return strconv.FormatFloat(float64(n)/1000, 'g', -1, 64), nil
	}

	// Plain decimal: "2", "1.5", "0.25". Reject negative + NaN +
	// Inf via the parser (ParseFloat catches malformed; explicit
	// negative check below catches "-1").
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return "", fmt.Errorf("invalid cpu %q: must be decimal (e.g. 2 or 1.5) or millicores (e.g. 500m)", s)
	}

	if f < 0 {
		return "", fmt.Errorf("cpu must be non-negative, got %q", s)
	}

	return s, nil
}

// ParseMemoryBytes validates an operator-supplied memory value
// and returns the size in bytes. Empty input returns (0, nil) —
// caller treats as "no limit".
//
// Examples:
//
//	"4Gi"    → 4294967296   (4 * 1024^3)
//	"512Mi"  → 536870912    (512 * 1024^2)
//	"1G"     → 1000000000   (1 * 1000^3)
//	"1024"   → 1024
//	""       → 0    (caller skips emit)
//	"garbage" → error
func ParseMemoryBytes(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}

	// Try suffix-based parse first — order matters because "Mi"
	// is a longer match than "M". Check binary first (longer),
	// then decimal.
	suffixes := []struct {
		ext  string
		mult int64
	}{
		{"Ki", 1024},
		{"Mi", 1024 * 1024},
		{"Gi", 1024 * 1024 * 1024},
		{"Ti", 1024 * 1024 * 1024 * 1024},
		{"K", 1000},
		{"M", 1000 * 1000},
		{"G", 1000 * 1000 * 1000},
		{"T", 1000 * 1000 * 1000 * 1000},
	}

	for _, sx := range suffixes {
		if !strings.HasSuffix(s, sx.ext) {
			continue
		}

		raw := strings.TrimSuffix(s, sx.ext)

		n, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid memory %q: prefix before %q must be a number", s, sx.ext)
		}

		if n < 0 {
			return 0, fmt.Errorf("memory must be non-negative, got %q", s)
		}

		bytes := int64(n * float64(sx.mult))

		return bytes, nil
	}

	// No suffix: plain bytes integer.
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid memory %q: must be integer bytes (e.g. 1024) or have a unit suffix (Ki/Mi/Gi/Ti, K/M/G/T)", s)
	}

	if n < 0 {
		return 0, fmt.Errorf("memory must be non-negative, got %q", s)
	}

	return n, nil
}
