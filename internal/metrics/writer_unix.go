//go:build unix

package metrics

import (
	"errors"
	"syscall"
)

// isENOSPC is the Unix shape of the "disk full" check. Pulled out
// behind a build tag so the package still compiles cleanly on
// non-Unix targets (developer machines that aren't the controller's
// production runtime).
func isENOSPC(err error) bool {
	return errors.Is(err, syscall.ENOSPC)
}
