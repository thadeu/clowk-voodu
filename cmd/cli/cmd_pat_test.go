package main

import (
	"testing"

	"go.voodu.clowk.in/internal/controller"
)

// `--scope=all` is a shorthand for the list AS IT IS TODAY, expanded before
// the request leaves. A token recorded as "all" would silently gain every
// scope added in a later release — its power growing without anybody deciding
// it should, and without the operator who minted it being able to see that it
// had.
func TestExpandAllNamesEveryScopeThatExistsNow(t *testing.T) {
	got := expandAll([]string{"all"})

	if len(got) != len(controller.KnownScopes()) {
		t.Fatalf("expandAll = %v, want one entry per known scope %v", got, controller.KnownScopes())
	}

	for _, want := range controller.KnownScopes() {
		found := false

		for _, g := range got {
			if g == string(want) {
				found = true

				break
			}
		}

		if !found {
			t.Errorf("scope %q missing from %v", want, got)
		}
	}

	// And never the literal word, which is what would make it a blank cheque.
	for _, g := range got {
		if g == "all" {
			t.Error(`"all" reached the request instead of being expanded`)
		}
	}
}

func TestExpandAllComposesWithNamedScopes(t *testing.T) {
	got := expandAll([]string{"read", "all", "read"})

	if len(got) != len(controller.KnownScopes()) {
		t.Errorf("expandAll = %v, want the deduplicated full set", got)
	}

	if got[0] != "read" {
		t.Errorf("an explicitly named scope should keep its place: %v", got)
	}
}

func TestExpandAllLeavesAnOrdinaryListAlone(t *testing.T) {
	got := expandAll([]string{"read", "deploy"})

	if len(got) != 2 || got[0] != "read" || got[1] != "deploy" {
		t.Errorf("expandAll = %v, want [read deploy]", got)
	}
}
