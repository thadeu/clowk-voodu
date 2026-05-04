// Tests for the k8s-style CPU + memory parsers. These pin the
// grammar surface — k8s parity is the contract, so the tests
// double as documentation of which value forms voodu accepts.

package k8svalues

import (
	"testing"
)

func TestParseCPU_PlainDecimal(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"2", "2"},
		{"1.5", "1.5"},
		{"0.25", "0.25"},
		{"0.1", "0.1"},
	}

	for _, tc := range cases {
		got, err := ParseCPU(tc.in)
		if err != nil {
			t.Errorf("ParseCPU(%q): unexpected err %v", tc.in, err)
			continue
		}

		if got != tc.want {
			t.Errorf("ParseCPU(%q): got %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestParseCPU_Millicores(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"500m", "0.5"},
		{"100m", "0.1"},
		{"1000m", "1"},
		{"250m", "0.25"},
		{"50m", "0.05"},
		{"2500m", "2.5"},
	}

	for _, tc := range cases {
		got, err := ParseCPU(tc.in)
		if err != nil {
			t.Errorf("ParseCPU(%q): unexpected err %v", tc.in, err)
			continue
		}

		if got != tc.want {
			t.Errorf("ParseCPU(%q): got %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestParseCPU_EmptyAllowed(t *testing.T) {
	got, err := ParseCPU("")
	if err != nil {
		t.Fatalf("empty should be no-op, got err: %v", err)
	}

	if got != "" {
		t.Errorf("empty should round-trip empty, got %q", got)
	}

	got, err = ParseCPU("   ")
	if err != nil || got != "" {
		t.Errorf("whitespace should normalise to empty, got %q err=%v", got, err)
	}
}

func TestParseCPU_Errors(t *testing.T) {
	cases := []string{
		"garbage",
		"-1",
		"-500m",
		"100.5m", // fractional millicores rejected (k8s convention)
		"abc500m",
		"500mi", // not k8s grammar (binary unit on cpu makes no sense)
	}

	for _, in := range cases {
		_, err := ParseCPU(in)
		if err == nil {
			t.Errorf("ParseCPU(%q): expected error, got nil", in)
		}
	}
}

func TestParseMemoryBytes_BinarySuffixes(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"1Ki", 1024},
		{"4Ki", 4 * 1024},
		{"512Mi", 512 * 1024 * 1024},
		{"4Gi", 4 * 1024 * 1024 * 1024},
		{"1Ti", 1024 * 1024 * 1024 * 1024},
	}

	for _, tc := range cases {
		got, err := ParseMemoryBytes(tc.in)
		if err != nil {
			t.Errorf("ParseMemoryBytes(%q): unexpected err %v", tc.in, err)
			continue
		}

		if got != tc.want {
			t.Errorf("ParseMemoryBytes(%q): got %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestParseMemoryBytes_DecimalSuffixes(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"1K", 1000},
		{"500K", 500_000},
		{"1M", 1_000_000},
		{"500M", 500_000_000},
		{"1G", 1_000_000_000},
		{"4G", 4_000_000_000},
		{"1T", 1_000_000_000_000},
	}

	for _, tc := range cases {
		got, err := ParseMemoryBytes(tc.in)
		if err != nil {
			t.Errorf("ParseMemoryBytes(%q): unexpected err %v", tc.in, err)
			continue
		}

		if got != tc.want {
			t.Errorf("ParseMemoryBytes(%q): got %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestParseMemoryBytes_PlainBytes(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"0", 0},
		{"1024", 1024},
		{"1048576", 1048576},
	}

	for _, tc := range cases {
		got, err := ParseMemoryBytes(tc.in)
		if err != nil {
			t.Errorf("ParseMemoryBytes(%q): unexpected err %v", tc.in, err)
			continue
		}

		if got != tc.want {
			t.Errorf("ParseMemoryBytes(%q): got %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestParseMemoryBytes_BinaryWinsOverDecimal(t *testing.T) {
	// "Mi" must NOT match the "M" rule — longer suffix wins.
	got, err := ParseMemoryBytes("100Mi")
	if err != nil {
		t.Fatal(err)
	}

	want := int64(100 * 1024 * 1024)
	if got != want {
		t.Errorf("100Mi resolved to %d (decimal), want %d (binary)", got, want)
	}
}

func TestParseMemoryBytes_EmptyAllowed(t *testing.T) {
	got, err := ParseMemoryBytes("")
	if err != nil {
		t.Fatalf("empty should be no-op, got err: %v", err)
	}

	if got != 0 {
		t.Errorf("empty should round-trip 0, got %d", got)
	}
}

func TestParseMemoryBytes_FractionalSuffixed(t *testing.T) {
	// "1.5Gi" is valid in k8s (fractional with suffix). Confirm
	// we accept and round to bytes.
	got, err := ParseMemoryBytes("1.5Gi")
	if err != nil {
		t.Fatal(err)
	}

	want := int64(1.5 * 1024 * 1024 * 1024)
	if got != want {
		t.Errorf("1.5Gi: got %d, want %d", got, want)
	}
}

func TestParseMemoryBytes_Errors(t *testing.T) {
	cases := []string{
		"garbage",
		"-1",
		"-1Gi",
		"abcGi",
		"4XB",       // unknown suffix
		"4 Gi",      // space inside (after trim, "4 Gi" still has internal space)
		"Gi",        // missing number
		"1024.5",    // fractional plain bytes (must be integer when no suffix)
	}

	for _, in := range cases {
		_, err := ParseMemoryBytes(in)
		if err == nil {
			t.Errorf("ParseMemoryBytes(%q): expected error, got nil", in)
		}
	}
}

