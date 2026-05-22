package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

// TestGeneratePAT_Format pins the token wire shape that the CLI
// shows to the operator at create time, the WebUI puts into its
// `Authorization: Bearer` header, and the middleware verifies.
// A regression in the shape (length / prefix / ID width) silently
// breaks every consumer; the test traps it before ship.
func TestGeneratePAT_Format(t *testing.T) {
	plain, rec, err := GeneratePAT([]Scope{ScopeRead}, "test")
	if err != nil {
		t.Fatalf("GeneratePAT: %v", err)
	}

	if !strings.HasPrefix(plain, "pat_") {
		t.Errorf("plain missing pat_ prefix: %q", plain)
	}

	wantLen := len("pat_") + patTokenBodyLen
	if got := len(plain); got != wantLen {
		t.Errorf("plain length: %d, want %d (pat_ + %d body chars)", got, wantLen, patTokenBodyLen)
	}

	if got := len(rec.ID); got != patTokenIDLen {
		t.Errorf("ID length: %d, want %d", got, patTokenIDLen)
	}

	// ID is the first patTokenIDLen chars AFTER the prefix.
	wantID := plain[len("pat_") : len("pat_")+patTokenIDLen]
	if rec.ID != wantID {
		t.Errorf("ID = %q, want %q (first %d chars after prefix)", rec.ID, wantID, patTokenIDLen)
	}

	if got := len(rec.HashHex); got != 64 {
		t.Errorf("HashHex length: %d, want 64 (sha256 hex)", got)
	}

	// HashHex must be sha256(plain) — confirm explicitly.
	expected := sha256.Sum256([]byte(plain))
	if rec.HashHex != hex.EncodeToString(expected[:]) {
		t.Errorf("HashHex does not match sha256(plain): got %q", rec.HashHex)
	}
}

// TestGeneratePAT_Uniqueness pins that successive generations produce
// distinct IDs. A weak random source (or a bug stamping the same ID)
// would collide silently and overwrite existing PATs in the store.
// Two consecutive calls is enough to spot a non-random source.
func TestGeneratePAT_Uniqueness(t *testing.T) {
	plain1, rec1, err := GeneratePAT([]Scope{ScopeRead}, "a")
	if err != nil {
		t.Fatal(err)
	}

	plain2, rec2, err := GeneratePAT([]Scope{ScopeRead}, "b")
	if err != nil {
		t.Fatal(err)
	}

	if plain1 == plain2 {
		t.Errorf("two GeneratePAT calls returned identical plain tokens — randomness broken")
	}

	if rec1.ID == rec2.ID {
		t.Errorf("two GeneratePAT calls returned identical IDs — randomness broken: %q", rec1.ID)
	}

	if rec1.HashHex == rec2.HashHex {
		t.Errorf("two GeneratePAT calls produced same HashHex — sha256 deterministic but inputs should differ")
	}
}

// TestGeneratePAT_RejectsEmptyScopes pins the "at least one scope"
// guarantee. Without it, the create endpoint could mint zero-scope
// tokens that pass middleware lookup (PAT exists) but fail every
// HasScope check — useless and confusing.
func TestGeneratePAT_RejectsEmptyScopes(t *testing.T) {
	if _, _, err := GeneratePAT(nil, ""); err == nil {
		t.Error("GeneratePAT with nil scopes: want error, got nil")
	}

	if _, _, err := GeneratePAT([]Scope{}, ""); err == nil {
		t.Error("GeneratePAT with empty scopes: want error, got nil")
	}
}

// TestGeneratePAT_RejectsUnknownScope pins that bad input fails
// loud. Silent acceptance of a typo (e.g. "reade") would mint a
// PAT that fails every scope check — operator wouldn't notice
// until they hit the API and got 403s.
func TestGeneratePAT_RejectsUnknownScope(t *testing.T) {
	_, _, err := GeneratePAT([]Scope{Scope("admin")}, "")
	if err == nil {
		t.Fatal("GeneratePAT with unknown scope: want error")
	}

	if !strings.Contains(err.Error(), "unknown scope") {
		t.Errorf("err: %v, want error mentioning 'unknown scope'", err)
	}
}

// TestGeneratePAT_NormalizesScopes pins that duplicates dedupe
// and order is deterministic (`read` before `actions`). Two
// semantically-equivalent inputs MUST produce byte-identical
// stored records so audit tooling / diff doesn't flap.
func TestGeneratePAT_NormalizesScopes(t *testing.T) {
	_, rec1, _ := GeneratePAT([]Scope{ScopeActions, ScopeRead}, "")
	_, rec2, _ := GeneratePAT([]Scope{ScopeRead, ScopeActions, ScopeRead}, "")

	if len(rec1.Scopes) != 2 || rec1.Scopes[0] != ScopeRead || rec1.Scopes[1] != ScopeActions {
		t.Errorf("rec1.Scopes: %v, want [read actions]", rec1.Scopes)
	}

	if len(rec2.Scopes) != 2 || rec2.Scopes[0] != ScopeRead || rec2.Scopes[1] != ScopeActions {
		t.Errorf("rec2.Scopes: %v, want [read actions] (duplicates deduped)", rec2.Scopes)
	}
}

// TestHashPAT_Deterministic + matches sha256. Pins the verification
// contract — the middleware computes HashPAT(incoming bearer) and
// compares to stored HashHex. A drift in hashing here breaks every
// PAT lookup.
func TestHashPAT_Deterministic(t *testing.T) {
	plain := "pat_DEADBEEF12345678901234567890"

	a := HashPAT(plain)
	b := HashPAT(plain)

	if a != b {
		t.Errorf("HashPAT not deterministic: %q vs %q", a, b)
	}

	want := sha256.Sum256([]byte(plain))
	if a != hex.EncodeToString(want[:]) {
		t.Errorf("HashPAT does not match sha256(plain)")
	}

	if len(a) != 64 {
		t.Errorf("HashPAT length: %d, want 64", len(a))
	}
}

// TestParsePATToken pins the parser used by the auth middleware to
// extract the ID from a Bearer header. Wrong prefix / wrong length
// MUST return ok=false; correct shape returns the ID slice.
func TestParsePATToken(t *testing.T) {
	plainGood, rec, _ := GeneratePAT([]Scope{ScopeRead}, "")

	cases := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{"valid token", plainGood, rec.ID, true},
		{"missing prefix", strings.TrimPrefix(plainGood, "pat_"), "", false},
		{"wrong prefix", "ghp_DEADBEEF12345678901234567890", "", false},
		{"too short", "pat_SHORT", "", false},
		{"too long", plainGood + "EXTRA", "", false},
		{"empty", "", "", false},
		{"just prefix", "pat_", "", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotID, gotOk := ParsePATToken(c.in)
			if gotOk != c.ok {
				t.Errorf("ParsePATToken(%q).ok = %v, want %v", c.in, gotOk, c.ok)
			}

			if gotID != c.want {
				t.Errorf("ParsePATToken(%q).id = %q, want %q", c.in, gotID, c.want)
			}
		})
	}
}

// TestParseScopes covers the CLI flag input path — `--scope=read,actions`
// gets parsed here. Edge cases (whitespace, duplicates, empty,
// unknown) are the entire failure surface.
func TestParseScopes(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    []Scope
		wantErr bool
	}{
		{"single read", "read", []Scope{ScopeRead}, false},
		{"single actions", "actions", []Scope{ScopeActions}, false},
		{"both ordered", "read,actions", []Scope{ScopeRead, ScopeActions}, false},
		{"both reversed normalises", "actions,read", []Scope{ScopeRead, ScopeActions}, false},
		{"with whitespace", " read , actions ", []Scope{ScopeRead, ScopeActions}, false},
		{"dedupes", "read,read,actions", []Scope{ScopeRead, ScopeActions}, false},

		{"empty string", "", nil, true},
		{"whitespace only", "   ", nil, true},
		{"single comma", ",", nil, true},
		{"unknown scope", "read,admin", nil, true},
		{"unknown alone", "writer", nil, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseScopes(c.in)
			if (err != nil) != c.wantErr {
				t.Fatalf("ParseScopes(%q): err=%v, wantErr=%v", c.in, err, c.wantErr)
			}

			if c.wantErr {
				return
			}

			if len(got) != len(c.want) {
				t.Fatalf("ParseScopes(%q): got %v, want %v", c.in, got, c.want)
			}

			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("ParseScopes(%q)[%d] = %q, want %q", c.in, i, got[i], c.want[i])
				}
			}
		})
	}
}

// TestPAT_HasScope pins the middleware's gate. Without a working
// HasScope, every request either gets through or none do — both
// catastrophic.
func TestPAT_HasScope(t *testing.T) {
	readOnly := PAT{Scopes: []Scope{ScopeRead}}
	actionsOnly := PAT{Scopes: []Scope{ScopeActions}}
	both := PAT{Scopes: []Scope{ScopeRead, ScopeActions}}
	none := PAT{}

	if !readOnly.HasScope(ScopeRead) {
		t.Error("read-only PAT should have read scope")
	}

	if readOnly.HasScope(ScopeActions) {
		t.Error("read-only PAT must NOT have actions scope")
	}

	if !actionsOnly.HasScope(ScopeActions) {
		t.Error("actions-only PAT should have actions scope")
	}

	if actionsOnly.HasScope(ScopeRead) {
		t.Error("actions-only PAT must NOT have read scope")
	}

	if !both.HasScope(ScopeRead) || !both.HasScope(ScopeActions) {
		t.Error("both-scopes PAT should have both")
	}

	if none.HasScope(ScopeRead) || none.HasScope(ScopeActions) {
		t.Error("scopeless PAT must NOT have any scope")
	}
}

// TestGeneratePAT_TrimsName pins the trim — operators paste names
// with trailing newlines / spaces all the time. Stored value is
// canonical for `vd pat list` rendering.
func TestGeneratePAT_TrimsName(t *testing.T) {
	_, rec, err := GeneratePAT([]Scope{ScopeRead}, "  webui-staging\n")
	if err != nil {
		t.Fatal(err)
	}

	if rec.Name != "webui-staging" {
		t.Errorf("Name not trimmed: %q", rec.Name)
	}
}
