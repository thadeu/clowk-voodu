package remote

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Info is where a command is going: the SSH destination for this
// invocation. BaseDir is the server's voodu root — currently
// constant at /opt/voodu, kept as a field so future per-host
// overrides don't require a struct change.
//
// Identity is the path to an SSH private key (e.g. an EC2 .pem
// file) when the remote URL embedded one via the
// `user@host:/path/to/key` form. Empty means "let ssh pick from
// ~/.ssh/config / agent / default keys" — the same behaviour
// every other CLI command preserves.
type Info struct {
	RemoteName string // git remote label that resolved to this info
	Host       string // user@hostname (passed verbatim to ssh)
	BaseDir    string // /opt/voodu by default
	Identity   string // optional ssh -i target, parsed out of the URL
}

// DefaultRemote is the name of the git remote voodu looks up first
// when no --remote / -r is given. Operators typically have one
// production target per repo, so "the voodu remote" is unambiguous
// and the explicit flag stays optional for the common case.
const DefaultRemote = "voodu"

// DefaultBaseDir mirrors paths.DefaultRoot on the server side. We don't
// import paths here to avoid a client-side dependency on server-side
// filesystem layout.
const DefaultBaseDir = "/opt/voodu"

// ParseRemoteURL parses the `user@host[:identity]` form that `git
// remote add` stores. The URL is not a real URL (no scheme) — voodu
// reuses the git remote config as a lightweight key/value store
// for SSH targets, nothing more. Normal git URLs (https://, git@
// github.com:..., etc.) are rejected so ListAll can scan a repo
// with mixed remotes and keep only the voodu ones.
//
// Two accepted shapes:
//
//	user@host                                 default key (~/.ssh/config / agent)
//	user@host:/path/to/key.pem                explicit ssh -i target
//	user@host:~/.ssh/ec2-prod.pem             tilde expanded at parse time
//
// The path discriminator is conservative: we only treat the suffix
// after `:` as an identity when it starts with `/`, `~`, `./`, or
// `../`. A bare token after `:` (e.g. `user@host:api`) was the
// legacy app shape — reject it loudly so an operator with stale
// git config gets a clear "update your remote URL" error instead
// of voodu silently treating "api" as an SSH key path.
func ParseRemoteURL(url string) (Info, error) {
	url = strings.TrimSpace(url)
	if url == "" {
		return Info{}, fmt.Errorf("empty remote URL")
	}

	// Reject real URL schemes and github-style SSH refs outright. The
	// test is conservative: anything containing "://" or ending in ".git"
	// is definitely not a voodu remote.
	if strings.Contains(url, "://") || strings.HasSuffix(url, ".git") {
		return Info{}, fmt.Errorf("invalid remote URL %q: want user@host or user@host:/path/to/key.pem", url)
	}

	if !strings.Contains(url, "@") {
		return Info{}, fmt.Errorf("invalid remote URL %q: host must be user@hostname", url)
	}

	host, identity, hasIdentity := strings.Cut(url, ":")

	if !hasIdentity {
		return Info{
			Host:    host,
			BaseDir: DefaultBaseDir,
		}, nil
	}

	if !looksLikeIdentityPath(identity) {
		return Info{}, fmt.Errorf("invalid remote URL %q: the part after ':' must be a path to an ssh key (start with /, ~, ./, or ../). Bare tokens are rejected — drop the suffix or rewrite as user@host:/path/to/key.pem", url)
	}

	expanded, err := expandHome(identity)
	if err != nil {
		return Info{}, fmt.Errorf("invalid remote URL %q: expand identity: %w", url, err)
	}

	return Info{
		Host:     host,
		BaseDir:  DefaultBaseDir,
		Identity: expanded,
	}, nil
}

// looksLikeIdentityPath reports whether s is path-shaped (vs. the
// legacy bare app token). Used by ParseRemoteURL to disambiguate
// `user@host:/path/to/key` (identity) from `user@host:api`
// (legacy, rejected).
func looksLikeIdentityPath(s string) bool {
	switch {
	case strings.HasPrefix(s, "/"),
		strings.HasPrefix(s, "~"),
		strings.HasPrefix(s, "./"),
		strings.HasPrefix(s, "../"):
		return true
	}

	return false
}

// expandHome resolves a leading `~` or `~/` to the current user's
// home directory. ssh's own `~` expansion only kicks in for
// shell-quoted paths in the config file; when we pass the path as
// a `-i` argument from Go's exec.Command, no shell is involved, so
// we have to expand it ourselves or ssh sees a literal `~/...`
// path that doesn't exist.
func expandHome(p string) (string, error) {
	if !strings.HasPrefix(p, "~") {
		return p, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}

	if p == "~" {
		return home, nil
	}

	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:]), nil
	}

	return p, nil
}

// Lookup returns the Info for a git remote by name. Errors when the
// remote is missing or its URL doesn't parse — both are real user
// problems the dispatcher should surface instead of silently skipping.
func Lookup(name string) (Info, error) {
	url, err := gitRemoteURL(name)
	if err != nil {
		return Info{}, err
	}

	info, err := ParseRemoteURL(url)
	if err != nil {
		return Info{}, err
	}

	info.RemoteName = name

	return info, nil
}

// Resolve picks the right remote for this invocation. Precedence:
//
//  1. --remote NAME flag wins
//  2. default remote "voodu"
//
// Returns (nil, nil) when no remote can be located — the caller
// treats that as "not forwarding", not as an error, because many
// commands are perfectly fine running locally (e.g. `voodu
// version`).
func Resolve(remoteFlag string) (*Info, error) {
	if remoteFlag != "" {
		info, err := Lookup(remoteFlag)
		if err != nil {
			return nil, fmt.Errorf("--remote %s: %w", remoteFlag, err)
		}

		return &info, nil
	}

	if info, err := Lookup(DefaultRemote); err == nil {
		return &info, nil
	}

	return nil, nil
}

// ExtractFlags pulls --remote / -r out of argv. Returns (remote,
// remainingArgs): the remote name for dispatcher bookkeeping, and a
// cleaned argv to send to the server (stripping the flag because it
// has no meaning there).
//
// -a/--app is NOT consumed — it stays in `rest` as an opaque argv
// entry so server-side commands (`config set -a api`, `logs -a api`)
// get it verbatim. It's purely server-facing now.
func ExtractFlags(args []string) (remote string, rest []string) {
	rest = make([]string, 0, len(args))

	i := 0
	for i < len(args) {
		tok := args[i]

		switch {
		case tok == "--remote", tok == "-r":
			if i+1 < len(args) {
				remote = args[i+1]
				i += 2

				continue
			}

			i++

		case strings.HasPrefix(tok, "--remote="):
			remote = strings.TrimPrefix(tok, "--remote=")
			i++

		case strings.HasPrefix(tok, "-r="):
			remote = strings.TrimPrefix(tok, "-r=")
			i++

		default:
			rest = append(rest, tok)
			i++
		}
	}

	return remote, rest
}

// ListAll returns every git remote that parses as a Voodu remote.
// Remotes that look like normal git URLs (https://, git://, ssh://…)
// are skipped — we only care about user@host:app triples.
func ListAll() ([]Info, error) {
	raw, err := gitRemoteVerbose()
	if err != nil {
		return nil, err
	}

	seen := map[string]bool{}

	var out []Info

	for _, line := range strings.Split(raw, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}

		name, url := fields[0], fields[1]

		if seen[name] {
			continue
		}

		seen[name] = true

		info, err := ParseRemoteURL(url)
		if err != nil {
			continue
		}

		info.RemoteName = name
		out = append(out, info)
	}

	return out, nil
}

// gitRemoteURL shells out to `git remote get-url NAME`. We don't use
// go-git here: git is already a client-side prerequisite (the user
// stores their voodu targets via `voodu remote add`, which is just a
// `git remote add` under the hood), and one shell-out per command is
// negligible.
func gitRemoteURL(name string) (string, error) {
	// Stderr is swallowed — callers handle the error themselves and we
	// don't want `no such remote` noise on every client-mode invocation
	// where the default `voodu` remote just happens to not exist.
	cmd := exec.Command("git", "remote", "get-url", name)

	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git remote %q: %w", name, err)
	}

	return strings.TrimSpace(string(out)), nil
}

func gitRemoteVerbose() (string, error) {
	cmd := exec.Command("git", "remote", "-v")

	out, err := cmd.Output()
	if err != nil {
		return "", err
	}

	return string(out), nil
}
