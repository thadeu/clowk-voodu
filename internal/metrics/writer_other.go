//go:build !unix

package metrics

// isENOSPC stub for non-Unix builds. The production controller
// only ships for Linux, so this branch exists purely so the
// package compiles on a Windows developer machine doing `go test`.
func isENOSPC(_ error) bool {
	return false
}
