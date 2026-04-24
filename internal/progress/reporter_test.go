package progress

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// TestTextReporterMatchesLegacyFormat locks in the byte-for-byte
// compatibility promise with the pre-NDJSON wire format. Old clients
// (and `tee build.log` humans) must see exactly the banners they saw
// before this package existed.
func TestTextReporterMatchesLegacyFormat(t *testing.T) {
	var buf bytes.Buffer

	r := NewTextReporter(&buf)

	r.Hello()
	r.StepStart("build", "Building release...")
	r.Log(LevelInfo, "#1 building")
	r.StepEnd("build", StatusOK, nil)
	r.Summary("Build completed")
	r.Result("deployment", "softphone", "web", "applied")
	r.Result("service", "", "router", "unchanged")
	_ = r.Close()

	got := buf.String()

	wants := []string{
		"-----> Building release...\n",
		"#1 building\n",
		"-----> Build completed\n",
		"deployment/softphone/web applied\n",
		"service/router unchanged\n",
	}

	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("text reporter missing %q in output:\n%s", w, got)
		}
	}

	// StepStart/StepEnd pair must not double-print: the StepEnd is a
	// no-op in text mode, so the "Building release..." banner appears
	// exactly once.
	if strings.Count(got, "-----> Building release...") != 1 {
		t.Errorf("StepStart must emit exactly one banner, got:\n%s", got)
	}

	// Hello is a no-op in text mode — no stray "hello" or protocol
	// string should leak into the banner stream.
	if strings.Contains(got, "hello") || strings.Contains(got, ProtocolVersion) {
		t.Errorf("text reporter must not emit Hello in text mode:\n%s", got)
	}
}

// TestJSONReporterEmitsOneEventPerLine is the core wire-format
// contract: every Reporter call produces exactly one line on stdout,
// and each line is a valid JSON object with the documented Type.
// Clients parse on `\n` — if a call ever wrote two lines or a partial
// JSON, the stream would desync for the rest of the run.
func TestJSONReporterEmitsOneEventPerLine(t *testing.T) {
	var buf bytes.Buffer

	r := NewJSONReporter(&buf)

	r.Hello()
	r.StepStart("build", "Building release...")
	r.Log(LevelInfo, "#1 building")
	r.StepEnd("build", StatusOK, nil)
	r.Result("deployment", "softphone", "web", "applied")
	r.Summary("Built web in 3s")

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")

	if len(lines) != 6 {
		t.Fatalf("expected 6 events, got %d:\n%s", len(lines), buf.String())
	}

	wantTypes := []EventType{
		EventHello,
		EventStepStart,
		EventLog,
		EventStepEnd,
		EventResult,
		EventSummary,
	}

	for i, line := range lines {
		var e Event

		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Errorf("line %d is not valid JSON: %v\n  line: %s", i, err, line)
			continue
		}

		if e.Type != wantTypes[i] {
			t.Errorf("line %d: type = %q, want %q", i, e.Type, wantTypes[i])
		}

		if e.Ts == "" {
			t.Errorf("line %d: Ts is empty, should be stamped by write()", i)
		}
	}
}

// TestJSONReporterHelloCarriesProtocol guards the handshake contract:
// clients read the first line, check .protocol, and either continue in
// NDJSON mode or fall back to text rendering. If this ever emitted
// empty or wrong protocol, every client would silently disable NDJSON.
func TestJSONReporterHelloCarriesProtocol(t *testing.T) {
	var buf bytes.Buffer

	r := NewJSONReporter(&buf)
	r.Hello()

	var e Event
	if err := json.Unmarshal([]byte(strings.TrimRight(buf.String(), "\n")), &e); err != nil {
		t.Fatal(err)
	}

	if e.Type != EventHello {
		t.Errorf("hello type = %q, want %q", e.Type, EventHello)
	}

	if e.Protocol != ProtocolVersion {
		t.Errorf("hello protocol = %q, want %q", e.Protocol, ProtocolVersion)
	}
}

// TestJSONReporterStepEndCarriesError makes sure error messages on a
// failed step reach the wire — clients need the Error field to render
// a meaningful ✗ line instead of a bare "something failed" placeholder.
func TestJSONReporterStepEndCarriesError(t *testing.T) {
	var buf bytes.Buffer

	r := NewJSONReporter(&buf)
	r.StepEnd("build", StatusFail, errors.New("docker exited 125"))

	var e Event
	if err := json.Unmarshal([]byte(strings.TrimRight(buf.String(), "\n")), &e); err != nil {
		t.Fatal(err)
	}

	if e.Status != StatusFail {
		t.Errorf("status = %q, want %q", e.Status, StatusFail)
	}

	if e.Error != "docker exited 125" {
		t.Errorf("error = %q, want %q", e.Error, "docker exited 125")
	}
}

// TestNewReporterFromEnvSelectsJSON verifies the env-based constructor
// picks JSONReporter exactly when VOODU_PROTOCOL matches the current
// version string. Any other value (empty, future version, typo) falls
// back to text — we never promise to speak a protocol we don't ship.
func TestNewReporterFromEnvSelectsJSON(t *testing.T) {
	var buf bytes.Buffer

	t.Setenv(EnvProtocol, ProtocolVersion)

	r := NewReporterFromEnv(&buf)

	if _, ok := r.(*JSONReporter); !ok {
		t.Errorf("with VOODU_PROTOCOL=%q, got %T, want *JSONReporter", ProtocolVersion, r)
	}

	t.Setenv(EnvProtocol, "ndjson/99")

	r2 := NewReporterFromEnv(&buf)

	if _, ok := r2.(*TextReporter); !ok {
		t.Errorf("with VOODU_PROTOCOL=ndjson/99 (unsupported), got %T, want *TextReporter", r2)
	}

	t.Setenv(EnvProtocol, "")

	r3 := NewReporterFromEnv(&buf)

	if _, ok := r3.(*TextReporter); !ok {
		t.Errorf("with VOODU_PROTOCOL empty, got %T, want *TextReporter", r3)
	}
}
