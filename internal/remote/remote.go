package remote

import (
	"fmt"
	"os/exec"
	"strings"
)

// Info is where a command is going: the SSH destination for this
// invocation. BaseDir is the server's voodu root — currently
// constant at /opt/voodu, kept as a field so future per-host
// overrides don't require a struct change.
//
// Identity (which app to act on) lives entirely in the HCL
// manifest (scope + name), not in the remote URL. The remote
// reduces to "which server do I ssh to".
type Info struct {
	RemoteName string // git remote label that resolved to this info
	Host       string // user@hostname (passed verbatim to ssh)
	BaseDir    string // /opt/voodu by default
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

// ParseRemoteURL parses the `user@host` form that `git remote add`
// stores. The URL is not a real URL (no scheme) — voodu reuses the git
// remote config as a lightweight key/value store for SSH targets,
// nothing more. Normal git URLs (https://, git@github.com:..., etc.)
// are rejected so ListAll can scan a repo with mixed remotes and keep
// only the voodu ones.
func ParseRemoteURL(url string) (Info, error) {
	url = strings.TrimSpace(url)
	if url == "" {
		return Info{}, fmt.Errorf("empty remote URL")
	}

	// Reject real URL schemes and github-style SSH refs outright. The
	// test is conservative: anything containing "://" or ending in ".git"
	// is definitely not a voodu remote.
	if strings.Contains(url, "://") || strings.HasSuffix(url, ".git") {
		return Info{}, fmt.Errorf("invalid remote URL %q: want user@host", url)
	}

	if !strings.Contains(url, "@") {
		return Info{}, fmt.Errorf("invalid remote URL %q: host must be user@hostname", url)
	}

	// Legacy `user@host:app` form — the :app is redundant now that HCL
	// carries the identity, so reject it loudly. No silent strip: the
	// operator's git config is stale and needs a manual fix (one-line
	// `git remote set-url NAME user@host`).
	if strings.Contains(url, ":") {
		return Info{}, fmt.Errorf("invalid remote URL %q: drop the ':app' suffix — HCL now owns the app identity. Update with: git remote set-url NAME user@host", url)
	}

	return Info{
		Host:    url,
		BaseDir: DefaultBaseDir,
	}, nil
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
