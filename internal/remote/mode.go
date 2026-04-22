// Package remote is the client-side glue that decides whether a CLI
// invocation runs locally or gets forwarded to a server over SSH.
//
// Three pieces fit together:
//
//   - mode.go — is this host acting as a client (forward) or a server
//     (run local)? Read from ~/.voodurc, Gokku-compatible format.
//   - remote.go — given --remote / -a / a default convention, find the
//     user@host:app triple to forward to (via git remotes).
//   - ssh.go — shell out to ssh(1) and proxy stdin/stdout/stderr plus
//     the exit code back to the caller.
//
// Everything here is intentionally process-level (shell out, parse rc
// file). Plugging golang.org/x/crypto/ssh buys correctness in exchange
// for reimplementing the OpenSSH config resolution users already have
// working. Not worth it for M5.5.
package remote

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Mode is the client-vs-server identity of this host.
type Mode string

const (
	ModeClient Mode = "client"
	ModeServer Mode = "server"
)

// RCFileName is the config file we read/write, matching the Gokku
// convention so operators migrating muscle-memory is preserved.
const RCFileName = ".voodurc"

// CurrentMode returns the effective mode. Missing file → client, which
// is the safe default: a fresh checkout of the CLI on a laptop is a
// client until `voodu setup` writes ModeServer on the actual server.
func CurrentMode() Mode {
	raw := ReadRCMode()

	if raw == string(ModeServer) {
		return ModeServer
	}

	return ModeClient
}

func IsClientMode() bool { return CurrentMode() == ModeClient }
func IsServerMode() bool { return CurrentMode() == ModeServer }

// RCPath is ~/.voodurc.
func RCPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}

	return filepath.Join(home, RCFileName)
}

// ReadRCMode parses the `mode=` line from ~/.voodurc. Missing file or
// missing key returns empty string — the caller decides what that
// means. Later lines win so `voodu setup` can just append.
func ReadRCMode() string {
	path := RCPath()
	if path == "" {
		return ""
	}

	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	mode := ""

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		if v, ok := strings.CutPrefix(line, "mode="); ok {
			mode = strings.TrimSpace(v)
		}
	}

	return mode
}

// WriteRCMode writes `mode=<m>` to ~/.voodurc, preserving any other
// lines that were already there. Creates the file if missing.
func WriteRCMode(m Mode) error {
	path := RCPath()
	if path == "" {
		return fmt.Errorf("cannot resolve home directory")
	}

	lines, _ := readLines(path)

	found := false

	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "mode=") {
			lines[i] = "mode=" + string(m)
			found = true
		}
	}

	if !found {
		lines = append(lines, "mode="+string(m))
	}

	return os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0644)
}

func readLines(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	return strings.Split(strings.TrimRight(string(raw), "\n"), "\n"), nil
}
