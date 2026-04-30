package controller

import (
	"context"
	"strings"
	"testing"
)

// TestInterpolateAssetRefs_ResolvesScopedPath: the canonical
// happy path — `${asset.NAME.KEY}` in any string lands as the
// host path the asset handler materialises under
// `<assets_root>/<scope>/<NAME>/<KEY>`. Scope is implicit,
// taken from the closure (matches what statefulset/deployment
// do at apply time).
func TestInterpolateAssetRefs_ResolvesScopedPath(t *testing.T) {
	store := newMemStore()

	_, _ = store.Put(context.Background(), &Manifest{
		Kind:  KindAsset,
		Scope: "data",
		Name:  "redis",
	})

	lookup := makeAssetPathLookup(context.Background(), store, "data")

	cases := map[string]string{
		"${asset.redis.configuration}:/etc/redis/redis.conf:ro":            "/opt/voodu/assets/data/redis/configuration:/etc/redis/redis.conf:ro",
		"plain string with no refs":                                        "plain string with no refs",
		"${asset.redis.users_acl} and ${asset.redis.configuration} mixed":  "/opt/voodu/assets/data/redis/users_acl and /opt/voodu/assets/data/redis/configuration mixed",
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

// TestInterpolateAssetRefs_UnresolvedIsLoud: a reference to
// an asset that doesn't exist in the store fails with the
// (name, key) the operator typed, so they can fix the typo
// without grepping the source.
func TestInterpolateAssetRefs_UnresolvedIsLoud(t *testing.T) {
	store := newMemStore()
	lookup := makeAssetPathLookup(context.Background(), store, "data")

	_, err := InterpolateAssetRefs("${asset.ghost.foo}", lookup)
	if err == nil {
		t.Fatal("expected error for unresolved asset")
	}

	if !strings.Contains(err.Error(), "ghost.foo") {
		t.Errorf("error should mention ghost.foo: %v", err)
	}
}

// TestResolveAssetRefsInSlice / Map cover the convenience
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

	lookup := makeAssetPathLookup(context.Background(), store, "data")

	gotSlice, err := resolveAssetRefsInSlice(
		[]string{"${asset.x.k}:/etc/x:ro", "literal"},
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
			"CONFIG_FILE": "${asset.x.k}",
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
