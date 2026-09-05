// Package-level note: trigger.go owns the PURE DATA shape of a deploy
// trigger. No I/O, no etcd, no HTTP — the same split pat.go makes, and for the
// same reason: the rules here are the ones a test must be able to exercise
// without standing anything up.
//
// WHAT A TRIGGER IS, AND WHAT IT IS NOT.
//
// A trigger is the operator's statement of TRUST: this repository, on this
// branch, may apply to these scopes on this box. It is created on the box, by
// the operator, with `vd deploy trigger create`. It is deliberately small and
// it changes almost never.
//
// It is NOT the deploy configuration. What actually gets applied — which
// manifest file, which paths are watched, which tags fire — lives in
// `.voodu/**/*.yml` INSIDE THE REPOSITORY, travels with the code, and is
// reviewed in a pull request like anything else.
//
// That split is invariant II of the deploy plane, and it is what a compromised
// control plane runs into: the SaaS can ask this box to deploy, but it cannot
// widen what the box will accept. Changing the trust statement means having
// access to the box; changing the deploy config means landing a commit on the
// pinned branch. Neither is something a stolen deploy token can do.

package controller

import (
	"fmt"
	"strings"
	"time"
)

// Trigger is one repository this box will deploy from.
type Trigger struct {
	// ID is the stable handle the control plane fires against. Generated on
	// create; never derived from the repository name, which can be renamed on
	// GitHub without anything telling us.
	ID string `json:"id"`

	// Repo is "owner/name" as GitHub spells it. Compared case-insensitively —
	// GitHub treats owner and repository names that way, and a trigger that
	// silently fails to match because somebody typed `Acme/Web` would be
	// debugged as "the webhook is broken".
	Repo string `json:"repo"`

	// Branch is PINNED, and it is the anchor of invariant III: every deploy
	// must name a commit that is an ancestor of this branch. A pull request
	// from a fork lives in the same object store and is reachable by SHA, so
	// without the ancestry check a token could apply code nobody reviewed.
	Branch string `json:"branch"`

	// AllowScopes bounds what a deploy may touch. A manifest declaring a scope
	// outside this list is refused — after the parse, on what will actually be
	// applied rather than on a declaration beside it.
	//
	// Empty means NOTHING is allowed, not everything. A trigger created
	// without scopes is an incomplete trigger, and the safe reading of an
	// incomplete permission is the narrow one.
	AllowScopes []string `json:"allow_scopes"`

	// Enabled lets an operator stop a repository from deploying without
	// destroying the trust statement — the difference between "pause this"
	// and "I no longer trust this repository", which are different intentions
	// and should not share a command.
	Enabled bool `json:"enabled"`

	CreatedAt time.Time `json:"created_at"`

	// LastFiredAt is observability, not state: it answers "is this trigger
	// actually in use" on a box with several. Nothing branches on it.
	LastFiredAt *time.Time `json:"last_fired_at,omitempty"`

	// LastBuilt maps a build-context path to the git subtree SHA that was last
	// built from it successfully.
	//
	// This one IS state, and it is what lets a deploy skip the download. A
	// subtree SHA changes only when something inside that directory changed,
	// so a commit touching the README leaves every build context's SHA alone —
	// and the archive never has to be fetched to discover that.
	//
	// Keyed by PATH and not by repository, because two workloads in one
	// monorepo change independently: the answer "does this need building" is
	// per context, and a single repository-wide marker would rebuild both
	// whenever either moved.
	LastBuilt map[string]string `json:"last_built,omitempty"`
}

// TriggerInput is what a caller supplies on create or update. Separate from
// Trigger so the fields the SERVER owns — ID, CreatedAt, LastFiredAt — are
// unreachable from a request body rather than merely ignored.
type TriggerInput struct {
	Repo        string   `json:"repo"`
	Branch      string   `json:"branch"`
	AllowScopes []string `json:"allow_scopes"`
	Enabled     *bool    `json:"enabled,omitempty"`
}

// ErrTriggerInvalid wraps every validation failure so the HTTP layer can map
// the whole class to 400 without matching on strings.
type ErrTriggerInvalid struct{ Reason string }

func (e ErrTriggerInvalid) Error() string { return "trigger: " + e.Reason }

// Normalize validates the input and returns the canonical form.
//
// Canonical and not merely valid: repo lowercased, scopes deduped and sorted,
// whitespace gone. Two operators typing the same trust statement differently
// must produce the same record, or "is this repository already configured"
// becomes a question nobody can answer by looking.
func (in TriggerInput) Normalize() (TriggerInput, error) {
	out := TriggerInput{Enabled: in.Enabled}

	repo := strings.ToLower(strings.TrimSpace(in.Repo))
	if err := validateRepo(repo); err != nil {
		return TriggerInput{}, err
	}

	out.Repo = repo

	branch := strings.TrimSpace(in.Branch)
	if branch == "" {
		return TriggerInput{}, ErrTriggerInvalid{"branch is required — it is what a deploy's commit must descend from"}
	}

	// Branch names are case-SENSITIVE in git, unlike repository names. Not
	// lowercased, and the asymmetry is deliberate rather than an oversight.
	out.Branch = branch

	scopes, err := normalizeAllowScopes(in.AllowScopes)
	if err != nil {
		return TriggerInput{}, err
	}

	out.AllowScopes = scopes

	return out, nil
}

// validateRepo checks the "owner/name" shape.
//
// Strict, because this string is interpolated into GitHub API URLs. A value
// carrying a slash too many, a `..`, or a leading slash would address a
// different endpoint than the one the code reads like it addresses.
func validateRepo(repo string) error {
	if repo == "" {
		return ErrTriggerInvalid{"repo is required, as owner/name"}
	}

	owner, name, found := strings.Cut(repo, "/")
	if !found {
		return ErrTriggerInvalid{fmt.Sprintf("repo %q must be owner/name", repo)}
	}

	for _, part := range []string{owner, name} {
		if part == "" || strings.ContainsAny(part, "/\\ \t") || part == "." || part == ".." {
			return ErrTriggerInvalid{fmt.Sprintf("repo %q must be owner/name", repo)}
		}
	}

	return nil
}

func normalizeAllowScopes(in []string) ([]string, error) {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))

	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}

		if strings.ContainsAny(s, "/ \t") {
			return nil, ErrTriggerInvalid{fmt.Sprintf("scope %q contains a character a scope cannot hold", s)}
		}

		if seen[s] {
			continue
		}

		seen[s] = true

		out = append(out, s)
	}

	if len(out) == 0 {
		return nil, ErrTriggerInvalid{"allow_scopes is required — a trigger that allows nothing can deploy nothing, so an empty list is a mistake rather than a policy"}
	}

	sortStrings(out)

	return out, nil
}

// AllowsScope reports whether this trigger permits applying to `scope`.
//
// The empty scope — what an unscoped kind carries — is matched literally,
// so allowing it is something an operator opts into by listing "" explicitly
// rather than something that falls out of a nil check.
func (t *Trigger) AllowsScope(scope string) bool {
	for _, allowed := range t.AllowScopes {
		if allowed == scope {
			return true
		}
	}

	return false
}

// RefusedScopes returns every scope in the set this trigger does not allow,
// in the order given.
//
// ALL of them, not the first: an operator who has to widen a trigger wants to
// do it once. Reporting one refusal per attempt turns a three-scope repository
// into three round trips through a deploy that fails.
func (t *Trigger) RefusedScopes(scopes []string) []string {
	var refused []string

	for _, s := range scopes {
		if !t.AllowsScope(s) {
			refused = append(refused, s)
		}
	}

	return refused
}

// MatchesRepo compares case-insensitively, the way GitHub treats these names.
func (t *Trigger) MatchesRepo(repo string) bool {
	return strings.EqualFold(t.Repo, strings.TrimSpace(repo))
}

// sortStrings is a tiny insertion sort, to keep this file free of imports it
// would otherwise need for a list that is never longer than a handful.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
