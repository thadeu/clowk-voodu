package controller

import (
	"context"
	"strings"
	"testing"
)

// TestInterpolateAssetRefs_ScopedAndUnscoped pins the
// dual-syntax contract:
//
//   - 3-segment ${asset.NAME.KEY} resolves against an
//     UNSCOPED asset (declared as `asset "name" { ... }`,
//     scope="" in storage).
//   - 4-segment ${asset.SCOPE.NAME.KEY} resolves against
//     a SCOPED asset (declared as `asset "scope" "name"
//     { ... }`).
//
// The same lookup is used for both — the regex routes the
// scope group "" or non-empty into the same code path.
func TestInterpolateAssetRefs_ScopedAndUnscoped(t *testing.T) {
	store := newMemStore()

	// Both forms coexisting: a scoped asset at (data, redis)
	// and an unscoped asset at ("", redis-shared). Different
	// names so each ref form addresses its own.
	_, _ = store.Put(context.Background(), &Manifest{
		Kind:  KindAsset,
		Scope: "data",
		Name:  "redis",
	})

	_, _ = store.Put(context.Background(), &Manifest{
		Kind:  KindAsset,
		Scope: "",
		Name:  "redis-shared",
	})

	lookup := makeAssetPathLookup(context.Background(), store)

	cases := map[string]string{
		// 4-segment, scoped explicit
		"${asset.data.redis.configuration}:/etc/redis/redis.conf:ro": "/opt/voodu/assets/data/redis/configuration:/etc/redis/redis.conf:ro",
		// 3-segment, unscoped global
		"${asset.redis-shared.cfg}:/etc/redis/redis.conf:ro":         "/opt/voodu/assets/redis-shared/cfg:/etc/redis/redis.conf:ro",
		// no refs — passthrough
		"plain string with no refs": "plain string with no refs",
		// mixed in one string
		"${asset.data.redis.users_acl} + ${asset.redis-shared.cfg}": "/opt/voodu/assets/data/redis/users_acl + /opt/voodu/assets/redis-shared/cfg",
	}

	for in, want := range cases {
		got, err := InterpolateAssetRefs(in, lookup)
		if err != nil {
			t.Errorf("interpolate %q: %v", in, err)
			continue
		}

		if got != want {
			t.Errorf("interpolate %q\n  got:  %q\n  want: %q", in, got, want)
		}
	}
}

// TestInterpolateAssetRefs_UnresolvedIsLoud: missing
// references error out with the (scope, name, key) the
// operator typed in the same shape they typed it (3 or 4
// segments). Helps fix typos without grepping the source.
func TestInterpolateAssetRefs_UnresolvedIsLoud(t *testing.T) {
	store := newMemStore()
	lookup := makeAssetPathLookup(context.Background(), store)

	// 3-segment unresolved
	if _, err := InterpolateAssetRefs("${asset.ghost.foo}", lookup); err == nil {
		t.Error("expected error for unresolved unscoped asset")
	} else if !strings.Contains(err.Error(), "ghost.foo") {
		t.Errorf("error should mention ghost.foo: %v", err)
	}

	// 4-segment unresolved — scope must appear in the error
	// so the operator can tell unscoped vs scoped misses
	// apart.
	_, err := InterpolateAssetRefs("${asset.data.ghost.foo}", lookup)
	if err == nil {
		t.Fatal("expected error for unresolved scoped asset")
	}

	if !strings.Contains(err.Error(), "data.ghost.foo") {
		t.Errorf("error should mention data.ghost.foo: %v", err)
	}
}

// TestResolveAssetRefsInCollections covers the convenience
// wrappers used by the deployment / statefulset handlers
// against operator-supplied volumes / env. Same lookup
// engine; this just confirms the slice + map paths don't
// short-circuit on empty inputs and pass errors through.
func TestResolveAssetRefsInCollections(t *testing.T) {
	store := newMemStore()

	_, _ = store.Put(context.Background(), &Manifest{
		Kind:  KindAsset,
		Scope: "data",
		Name:  "x",
	})

	lookup := makeAssetPathLookup(context.Background(), store)

	gotSlice, err := resolveAssetRefsInSlice(
		[]string{"${asset.data.x.k}:/etc/x:ro", "literal"},
		lookup,
	)
	if err != nil {
		t.Fatal(err)
	}

	if gotSlice[0] != "/opt/voodu/assets/data/x/k:/etc/x:ro" {
		t.Errorf("slice[0]: %q", gotSlice[0])
	}

	if gotSlice[1] != "literal" {
		t.Errorf("slice[1]: %q", gotSlice[1])
	}

	gotMap, err := resolveAssetRefsInMap(
		map[string]string{
			"CONFIG_FILE": "${asset.data.x.k}",
			"FOO":         "bar",
		},
		lookup,
	)
	if err != nil {
		t.Fatal(err)
	}

	if gotMap["CONFIG_FILE"] != "/opt/voodu/assets/data/x/k" {
		t.Errorf("map CONFIG_FILE: %q", gotMap["CONFIG_FILE"])
	}

	if gotMap["FOO"] != "bar" {
		t.Errorf("map FOO: %q", gotMap["FOO"])
	}
}
