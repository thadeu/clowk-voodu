package activity

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func mustWriter(t *testing.T) (*Writer, string) {
	t.Helper()

	dir := t.TempDir()

	w, err := NewWriter(dir, nil)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	t.Cleanup(func() { w.Close() })

	return w, dir
}

func readLines(t *testing.T, dir, date string) []Record {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(dir, FileName(date)))
	if err != nil {
		t.Fatalf("read %s: %v", date, err)
	}

	var out []Record

	for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
		if len(line) == 0 {
			continue
		}

		var rec Record

		if err := json.Unmarshal(line, &rec); err != nil {
			t.Fatalf("unmarshal %q: %v", line, err)
		}

		out = append(out, rec)
	}

	return out
}

// The atomicity contract the Writer doc describes: one Write per line against
// an O_APPEND fd. If somebody "improves" this with a bufio.Writer, a line gets
// split across two syscalls and this test catches it.
func TestWriterConcurrentLinesAreNeverTorn(t *testing.T) {
	w, dir := mustWriter(t)

	const writers, each = 16, 40

	ts := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)

	var wg sync.WaitGroup

	for i := 0; i < writers; i++ {
		wg.Add(1)

		go func(i int) {
			defer wg.Done()

			for j := 0; j < each; j++ {
				rec := Record{
					ID:     "id",
					Ts:     ts,
					Event:  EventDone,
					Action: ActionApply,
					Origin: OriginCLI,
					// Padding so a torn line is very likely to be detectable
					// rather than accidentally still parsing.
					Name: strings.Repeat("x", 200),
				}

				if err := w.Write(rec); err != nil {
					t.Errorf("write: %v", err)
				}
			}
		}(i)
	}

	wg.Wait()

	recs := readLines(t, dir, "2026-09-02")

	if len(recs) != writers*each {
		t.Fatalf("got %d lines, want %d", len(recs), writers*each)
	}
}

// The file is chosen by the record's OWN timestamp, not by whatever handle is
// open — so an action that finishes after midnight lands in the right day and
// cannot race the cleanup pass gzipping yesterday's file.
func TestWriterPicksFileByRecordTimestamp(t *testing.T) {
	w, dir := mustWriter(t)

	before := time.Date(2026, 9, 2, 23, 59, 59, 0, time.UTC)
	after := time.Date(2026, 9, 3, 0, 0, 1, 0, time.UTC)

	if err := w.Write(Record{ID: "a", Ts: before, Event: EventStarted, Action: ActionApply}); err != nil {
		t.Fatal(err)
	}

	if err := w.Write(Record{ID: "a", Ts: after, Event: EventFinished, Action: ActionApply}); err != nil {
		t.Fatal(err)
	}

	if got := readLines(t, dir, "2026-09-02"); len(got) != 1 || got[0].Event != EventStarted {
		t.Fatalf("2026-09-02: %+v", got)
	}

	if got := readLines(t, dir, "2026-09-03"); len(got) != 1 || got[0].Event != EventFinished {
		t.Fatalf("2026-09-03: %+v", got)
	}
}

func TestNormalizeOriginDefaultsToAPI(t *testing.T) {
	cases := map[string]Origin{
		"cli":          OriginCLI,
		"ssh":          OriginSSH,
		"receive_pack": OriginReceivePack,
		"deploy_plane": OriginDeployPlane,
		"api":          OriginAPI,
		// An unrecognised label must not invent a category.
		"":             OriginAPI,
		"nonsense":     OriginAPI,
		"CLI":          OriginAPI,
		"deploy-plane": OriginAPI,
	}

	for in, want := range cases {
		if got := NormalizeOrigin(in); got != want {
			t.Errorf("NormalizeOrigin(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWriteDefaultsOriginAndTimestamp(t *testing.T) {
	w, dir := mustWriter(t)

	if err := w.Write(Record{ID: "x", Event: EventDone, Action: ActionDelete}); err != nil {
		t.Fatal(err)
	}

	recs := readLines(t, dir, time.Now().UTC().Format(dateLayout))

	if len(recs) != 1 {
		t.Fatalf("got %d records", len(recs))
	}

	if recs[0].Origin != OriginAPI {
		t.Errorf("origin = %q, want %q", recs[0].Origin, OriginAPI)
	}

	if recs[0].Ts.IsZero() {
		t.Error("ts was not defaulted")
	}
}

// The whole point of ValueDigest: it distinguishes "actually changed" from
// "re-set the same thing" WITHOUT the value ever being written.
func TestDigestValueIsStableAndNotTheValue(t *testing.T) {
	secret := "postgres://user:hunter2@db/prod"

	a := DigestValue(secret)
	if a != DigestValue(secret) {
		t.Error("digest is not stable")
	}

	if a == DigestValue(secret+" ") {
		t.Error("digest did not change for a different value")
	}

	if strings.Contains(a, "hunter2") || strings.Contains(a, "postgres") {
		t.Fatalf("digest leaked the value: %q", a)
	}
}

// A config record must carry the key and the digest, and nothing else about the
// value. This is the test that stops somebody adding a `value` field later
// "just for debugging".
func TestConfigRecordNeverCarriesTheValue(t *testing.T) {
	w, dir := mustWriter(t)

	const secret = "super-secret-token"

	err := w.Write(Record{
		ID:     "c1",
		Ts:     time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC),
		Event:  EventDone,
		Action: ActionConfigSet,
		Origin: OriginCLI,
		Scope:  "acme",
		ConfigKeys: []ConfigChange{
			{Key: "DATABASE_URL", ValueDigest: DigestValue(secret)},
		},
		Status: StatusSucceeded,
	})
	if err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, FileName("2026-09-02")))
	if err != nil {
		t.Fatal(err)
	}

	if bytes.Contains(raw, []byte(secret)) {
		t.Fatal("the config value reached the activity file")
	}

	if !bytes.Contains(raw, []byte("DATABASE_URL")) {
		t.Fatal("the config key should be recorded")
	}
}
