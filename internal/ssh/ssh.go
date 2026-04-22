// Package ssh wraps the system ssh/scp binaries. It exists so the rest of
// the codebase can stay agnostic of which SSH client is used — the day we
// swap to golang.org/x/crypto/ssh we only change this package.
package ssh

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Exec runs a shell command on the remote host and streams its
// stdout/stderr to the local process. `host` is in the form `user@host`.
func Exec(host, command string) error {
	if host == "" {
		return fmt.Errorf("ssh: empty host")
	}

	clean := strings.ReplaceAll(command, " --remote", "")
	clean = strings.ReplaceAll(clean, "--remote", "")

	cmd := exec.Command("ssh", host, clean)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin

	return cmd.Run()
}

// Output runs a shell command on the remote host and returns captured
// stdout+stderr. Used when the caller needs to parse the response.
func Output(host, command string) ([]byte, error) {
	if host == "" {
		return nil, fmt.Errorf("ssh: empty host")
	}

	cmd := exec.Command("ssh", host, command)
	return cmd.CombinedOutput()
}

// CopyToRemote uploads a local file to remote:dest via scp.
func CopyToRemote(host, localPath, remotePath string) error {
	cmd := exec.Command("scp", localPath, fmt.Sprintf("%s:%s", host, remotePath))
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd.Run()
}

// CopyFromRemote downloads remote:src to localPath via scp.
func CopyFromRemote(host, remotePath, localPath string) error {
	cmd := exec.Command("scp", fmt.Sprintf("%s:%s", host, remotePath), localPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd.Run()
}
