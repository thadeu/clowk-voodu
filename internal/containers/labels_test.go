package containers

import (
	"strings"
	"testing"
)

// TestContainerNameScoped pins the post-M0 naming convention. The dot
// separator before the replica id is what distinguishes "scope-name
// boundary" (hyphen) from "replica boundary" (dot). If this changes,
// every parser/inspector that splits on it has to change in lockstep.
func TestContainerNameScoped(t *testing.T) {
	got := ContainerName("softphone", "web", "a3f9")
	want := "softphone-web.a3f9"

	if got != want {
		t.Errorf("ContainerName(scoped) = %q, want %q", got, want)
	}
}

func TestContainerNameUnscoped(t *testing.T) {
	got := ContainerName("", "postgres", "b1c2")
	want := "postgres.b1c2"

	if got != want {
		t.Errorf("ContainerName(unscoped) = %q, want %q", got, want)
	}
}

// TestNewReplicaID guards format invariants: 4-char lowercase hex.
// docker name validation rejects upper-case-only suffixes in some
// edge cases historically, and we want the visual to be uniform.
func TestNewReplicaID(t *testing.T) {
	id := NewReplicaID()

	if len(id) != 4 {
		t.Fatalf("replica id length = %d, want 4 (got %q)", len(id), id)
	}

	for _, r := range id {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			t.Errorf("non-hex char %q in replica id %q", r, id)
		}
	}
}

// TestNewReplicaIDDifferentEachCall guards that we don't accidentally
// derive the ID from a clock or counter that gets called too fast.
// Collisions are possible (16 bits of space) but two consecutive calls
// returning the same value would be a deterministic-source bug.
func TestNewReplicaIDDifferentEachCall(t *testing.T) {
	a := NewReplicaID()
	b := NewReplicaID()

	if a == b {
		// Birthday paradox: P(collision) ≈ 1/65536. A repeat in two
		// consecutive calls is overwhelmingly a rand-source bug, not
		// genuine random collision.
		t.Errorf("two consecutive replica ids identical: %q — rand source broken?", a)
	}
}

// TestBuildLabelsAllFields exercises the happy path: an identity with
// every field populated produces the full set of `--label k=v` flags
// in a stable order. Tests that read the result later (`voodu get
// pods`) depend on the keys being present, not the order — but stable
// order keeps test diffs sane.
func TestBuildLabelsAllFields(t *testing.T) {
	id := Identity{
		Kind:         KindDeployment,
		Scope:        "softphone",
		Name:         "web",
		ReplicaID:    "a3f9",
		ManifestHash: "deadbeef",
		CreatedAt:    "2026-04-24T10:00:00Z",
	}

	got := BuildLabels(id)

	want := []string{
		"createdby=voodu",
		"voodu.kind=deployment",
		"voodu.scope=softphone",
		"voodu.name=web",
		"voodu.replica_id=a3f9",
		"voodu.manifest_hash=deadbeef",
		"voodu.created_at=2026-04-24T10:00:00Z",
		// Role defaults to Kind when Identity.Role is empty,
		// so every voodu container ends up with a non-empty
		// voodu.role label for `vd get pd` grouping.
		"voodu.role=deployment",
	}

	if len(got) != len(want) {
		t.Fatalf("BuildLabels length = %d, want %d: %v", len(got), len(want), got)
	}

	for i, w := range want {
		if got[i] != w {
			t.Errorf("label[%d] = %q, want %q", i, got[i], w)
		}
	}
}

// TestBuildLabelsSkipsEmpty proves that the zero-value Identity still
// produces a usable label set (just createdby). Important: docker run
// rejects `--label key=` (empty value) on some daemons, so emitting
// empty labels would brick container creation.
func TestBuildLabelsSkipsEmpty(t *testing.T) {
	got := BuildLabels(Identity{})

	if len(got) != 1 || got[0] != "createdby=voodu" {
		t.Errorf("BuildLabels(empty) = %v, want [createdby=voodu]", got)
	}

	for _, lbl := range got {
		if strings.HasSuffix(lbl, "=") {
			t.Errorf("empty-value label leaked: %q", lbl)
		}
	}
}

// TestParseLabelsRoundTrip ensures BuildLabels → ParseLabels recovers
// the original Identity. This is the canonical sanity check that the
// label key constants are spelled the same on both sides.
//
// Non-statefulset kinds carry ReplicaOrdinal=-1 as the "not set"
// sentinel — the label is never emitted for them, so ParseLabels
// has nothing to recover from and leaves the field at -1.
func TestParseLabelsRoundTrip(t *testing.T) {
	want := Identity{
		Kind:           KindJob,
		Scope:          "softphone",
		Name:           "migrate",
		ReplicaID:      "1234",
		ManifestHash:   "feedface",
		CreatedAt:      "2026-04-24T10:00:00Z",
		ReplicaOrdinal: -1,
		// BuildLabels defaults Role to Kind when not set, and
		// ParseLabels recovers it. Set the expected value here
		// so the round-trip equality holds.
		Role: KindJob,
	}

	flags := BuildLabels(want)

	// Convert "k=v" flags back to a label map (mimics what docker
	// inspect would return).
	labelMap := make(map[string]string, len(flags))

	for _, f := range flags {
		eq := strings.IndexByte(f, '=')
		if eq < 0 {
			t.Fatalf("malformed label flag: %q", f)
		}

		labelMap[f[:eq]] = f[eq+1:]
	}

	got, ok := ParseLabels(labelMap)
	if !ok {
		t.Fatal("ParseLabels rejected its own output")
	}

	if got != want {
		t.Errorf("round-trip lost data:\n got:  %+v\n want: %+v", got, want)
	}
}

// TestParseLabelsRoundTripStatefulset locks down ordinal recovery
// for statefulset pods specifically — the only kind that emits
// LabelReplicaOrdinal. Without this test, a regression that
// stopped emitting or recovering the label would only surface
// when an actual postgres/redis cluster lost its ordering.
func TestParseLabelsRoundTripStatefulset(t *testing.T) {
	want := Identity{
		Kind:           KindStatefulset,
		Scope:          "data",
		Name:           "pg",
		ReplicaID:      "2",
		ReplicaOrdinal: 2,
		ManifestHash:   "deadbeef",
		CreatedAt:      "2026-04-28T12:00:00Z",
		// Default role mirrors Kind.
		Role: KindStatefulset,
	}

	flags := BuildLabels(want)

	labelMap := make(map[string]string, len(flags))
	for _, f := range flags {
		eq := strings.IndexByte(f, '=')
		if eq < 0 {
			t.Fatalf("malformed label flag: %q", f)
		}

		labelMap[f[:eq]] = f[eq+1:]
	}

	got, ok := ParseLabels(labelMap)
	if !ok {
		t.Fatal("ParseLabels rejected statefulset output")
	}

	if got != want {
		t.Errorf("statefulset round-trip lost data:\n got:  %+v\n want: %+v", got, want)
	}

	if n, ok := got.Ordinal(); !ok || n != 2 {
		t.Errorf("Identity.Ordinal() = (%d,%v), want (2,true)", n, ok)
	}
}

// TestParseLabelsRejectsNonVoodu locks down the gate: a container
// without the createdby=voodu marker is foreign and must be filtered
// out, even if it happens to carry one of our other label keys.
func TestParseLabelsRejectsNonVoodu(t *testing.T) {
	cases := []map[string]string{
		nil,
		{},
		{"voodu.kind": "deployment"},                      // no createdby
		{"createdby": "other", "voodu.kind": "deployment"}, // wrong umbrella value
	}

	for _, m := range cases {
		if _, ok := ParseLabels(m); ok {
			t.Errorf("ParseLabels accepted non-voodu labels: %v", m)
		}
	}
}

func TestIdentityMatches(t *testing.T) {
	id := Identity{Kind: KindDeployment, Scope: "softphone", Name: "web", ReplicaID: "a3f9"}

	if !id.Matches(KindDeployment, "softphone", "web") {
		t.Error("Matches() rejected its own tuple")
	}

	if id.Matches(KindJob, "softphone", "web") {
		t.Error("Matches() ignored Kind difference")
	}

	if id.Matches(KindDeployment, "other", "web") {
		t.Error("Matches() ignored Scope difference")
	}

	if id.Matches(KindDeployment, "softphone", "other") {
		t.Error("Matches() ignored Name difference")
	}
}

// TestLegacyContainerName guards the M0 migration path: pre-M0
// containers are unlabeled, so the reconciler detects them by name
// pattern. Once detected they get replaced with M0-labeled siblings.
func TestLegacyContainerName(t *testing.T) {
	for _, c := range []struct {
		name, app string
		want      bool
	}{
		// Bare app name (oldest pre-slot voodu).
		{"softphone-web", "softphone-web", true},
		// Numeric slot suffix (post-slot, pre-M0).
		{"softphone-web-0", "softphone-web", true},
		{"softphone-web-12", "softphone-web", true},
		// New M0 naming — must NOT match (caller will see it via labels).
		{"softphone-web.a3f9", "softphone-web", false},
		// Sidecar with text suffix — preserved (legacy detector
		// must not eat unrelated containers).
		{"softphone-web-sidecar", "softphone-web", false},
		// Empty app guard.
		{"softphone-web-0", "", false},
		// Different app prefix.
		{"other-web-0", "softphone-web", false},
	} {
		if got := LegacyContainerName(c.name, c.app); got != c.want {
			t.Errorf("LegacyContainerName(%q, %q) = %v, want %v", c.name, c.app, got, c.want)
		}
	}
}

// TestBuildLabels_RoleDefaultsToKind pins the convention that
// every voodu container ends up with a non-empty voodu.role label,
// defaulting to Kind when the caller doesn't override. `vd get pd`
// uses this to group output by role.
func TestBuildLabels_RoleDefaultsToKind(t *testing.T) {
	cases := []struct {
		kind     string
		wantRole string
	}{
		{KindDeployment, "deployment"},
		{KindStatefulset, "statefulset"},
		{KindJob, "job"},
		{KindCronJob, "cronjob"},
	}

	for _, tc := range cases {
		labels := BuildLabels(Identity{
			Kind:  tc.kind,
			Scope: "test",
			Name:  "x",
		})

		want := "voodu.role=" + tc.wantRole

		found := false
		for _, l := range labels {
			if l == want {
				found = true
				break
			}
		}

		if !found {
			t.Errorf("kind %q: expected label %q, got: %v", tc.kind, want, labels)
		}
	}
}

// TestBuildLabels_RoleExplicitOverridesKind covers the override
// path: callers (release pipeline, voodu-postgres backup capture)
// set Identity.Role explicitly so the container groups under their
// custom category in `vd get pd`.
func TestBuildLabels_RoleExplicitOverridesKind(t *testing.T) {
	labels := BuildLabels(Identity{
		Kind:  KindJob,
		Scope: "clowk-lp",
		Name:  "release-abc",
		Role:  "release",
	})

	for _, l := range labels {
		if l == "voodu.role=release" {
			return
		}

		if l == "voodu.role=job" {
			t.Errorf("explicit Role=release was overridden by Kind default: %v", labels)
		}
	}

	t.Errorf("expected voodu.role=release in labels: %v", labels)
}

// TestParseLabels_RoundtripsRole pins that ParseLabels recovers
// the role from the docker label map. Used by the controller's
// pods listing endpoint to surface the role to the CLI for
// grouping.
func TestParseLabels_RoundtripsRole(t *testing.T) {
	id := Identity{
		Kind:  KindJob,
		Scope: "clowk-lp",
		Name:  "db.bk.b001",
		Role:  "backup",
	}

	labels := BuildLabels(id)

	// Convert label slice to map (mirrors what docker hands back).
	m := map[string]string{}
	for _, l := range labels {
		for i := 0; i < len(l); i++ {
			if l[i] == '=' {
				m[l[:i]] = l[i+1:]
				break
			}
		}
	}

	parsed, ok := ParseLabels(m)
	if !ok {
		t.Fatal("ParseLabels failed on freshly-built label set")
	}

	if parsed.Role != "backup" {
		t.Errorf("Role: got %q, want backup", parsed.Role)
	}
}
