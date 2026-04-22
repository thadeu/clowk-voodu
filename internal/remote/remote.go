package remote

import (
	"fmt"
	"os/exec"
	"strings"
)

// Info is where a command is going: the SSH destination plus the app
// name to use on the far side. BaseDir is the server's Voodu root —
// currently constant, kept as a field because the Gokku layout had it
// configurable and we may want that back.
type Info struct {
	RemoteName string // git remote label that resolved to this info
	Host       string // user@hostname (passed verbatim to ssh)
	App        string // app name as stored on the server
	BaseDir    string // /opt/voodu by default
}

// DefaultRemote is the convention that `git push voodu` goes to the
// primary server. Gokku used "gokku"; we keep the parallel.
const DefaultRemote = "voodu"

// DefaultBaseDir mirrors paths.DefaultRoot on the server side. We don't
// import paths here to avoid a client-side dependency on server-side
// filesystem layout.
const DefaultBaseDir = "/opt/voodu"

// ParseRemoteURL parses the `user@host:app` form that `git remote add`
// stores. The URL is not a real URL (no scheme) — it mirrors the
// scp-like Git syntax, which keeps `git push` working for free.
func ParseRemoteURL(url string) (Info, error) {
	url = strings.TrimSpace(url)
	if url == "" {
		return Info{}, fmt.Errorf("empty remote URL")
	}

	idx := strings.Index(url, ":")
	if idx <= 0 || idx == len(url)-1 {
		return Info{}, fmt.Errorf("invalid remote URL %q: want user@host:app", url)
	}

	host := url[:idx]
	app := url[idx+1:]

	if !strings.Contains(host, "@") {
		return Info{}, fmt.Errorf("invalid remote URL %q: host must be user@hostname", url)
	}

	return Info{
		Host:    host,
		App:     app,
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

// Resolve picks the right remote for this invocation. The precedence
// mirrors Gokku so existing muscle memory works:
//
//  1. --remote NAME flag wins
//  2. -a APP flag: we try a remote literally named APP
//  3. default remote "voodu"
//
// Returns (nil, nil) when no remote can be located — the caller treats
// that as "not forwarding", not as an error, because many commands are
// perfectly fine running locally (e.g. `voodu version`).
func Resolve(remoteFlag, appFlag string) (*Info, error) {
	if remoteFlag != "" {
		info, err := Lookup(remoteFlag)
		if err != nil {
			return nil, fmt.Errorf("--remote %s: %w", remoteFlag, err)
		}

		return &info, nil
	}

	if appFlag != "" {
		if info, err := Lookup(appFlag); err == nil {
			return &info, nil
		}
		// Fall through to default if the app-named remote doesn't exist;
		// -a can legitimately reference something the server knows about
		// that has no corresponding git remote (rare, but possible).
	}

	if info, err := Lookup(DefaultRemote); err == nil {
		if appFlag != "" {
			info.App = appFlag
		}

		return &info, nil
	}

	return nil, nil
}

// ExtractFlags pulls --remote and -a/--app out of argv. They are
// returned as (remote, app, remainingArgs). The dispatcher needs both
// forms: the values to pick a remote, and a cleaned argv to send to
// the server (stripping --remote because it has no meaning there).
func ExtractFlags(args []string) (remote, app string, rest []string) {
	rest = make([]string, 0, len(args))

	i := 0
	for i < len(args) {
		tok := args[i]

		switch {
		case tok == "--remote":
			if i+1 < len(args) {
				remote = args[i+1]
				i += 2

				continue
			}

			i++

		case strings.HasPrefix(tok, "--remote="):
			remote = strings.TrimPrefix(tok, "--remote=")
			i++

		case tok == "-a" || tok == "--app":
			if i+1 < len(args) {
				app = args[i+1]
				rest = append(rest, tok, args[i+1])
				i += 2

				continue
			}

			rest = append(rest, tok)
			i++

		case strings.HasPrefix(tok, "--app="):
			app = strings.TrimPrefix(tok, "--app=")
			rest = append(rest, tok)
			i++

		default:
			rest = append(rest, tok)
			i++
		}
	}

	return remote, app, rest
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
// go-git here: the client-side CLI already assumes `git` is on PATH
// (users run `git push voodu main`), and one shell-out per command is
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
