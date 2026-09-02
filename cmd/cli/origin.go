package main

import (
	"net/http"
	"os"

	"go.voodu.clowk.in/internal/activity"
	"go.voodu.clowk.in/internal/clientinfo"
	"go.voodu.clowk.in/internal/remote"
)

// requestOrigin is the value this invocation declares in X-Voodu-Origin.
//
// The controller cannot work this out on its own: a local `vd apply`, one
// forwarded over SSH, and one triggered by a git push all arrive as the same
// POST from the same binary. Without the caller declaring it, every row in the
// activity trail would read `api` and the history could not answer "who did
// this", which is most of the point.
//
// Precedence: the process-level override a subcommand set (receive-pack),
// then VOODU_ORIGIN from the environment (which SSH forwarding injects), then
// `cli` — a plain invocation on somebody's laptop.
//
// Normalised through the same function the controller uses, so an unknown
// value degrades to `api` on both ends rather than being rejected here.
func requestOrigin() string {
	if originOverride != "" {
		return string(activity.NormalizeOrigin(originOverride))
	}

	if v := os.Getenv(remote.OriginEnv); v != "" {
		return string(activity.NormalizeOrigin(v))
	}

	return string(activity.OriginCLI)
}

// originOverride is set by a subcommand that knows better than the
// environment what it is. Only receive-pack does today: it runs over SSH, so
// VOODU_ORIGIN says `ssh`, but "a git push deployed this" is the more useful
// fact and the trail should say so.
var originOverride string

// setOriginHeader stamps the origin on a controller request.
//
// A helper rather than the header set inline at each call site, so the one
// import and the one constant live here and adding a new controller call is a
// single line that cannot get the header name wrong.
func setOriginHeader(req *http.Request) {
	req.Header.Set(activity.OriginHeader, requestOrigin())

	// Two facts the CLIENT resolved and passed over the SSH env, forwarded
	// here verbatim. This binary cannot re-derive either one: a lookup from
	// the box returns the box's own address, and the file names were replaced
	// with `-f -` before the command left the laptop.
	//
	// Passed through opaquely — decoded only by the controller, which is the
	// one place that has to understand them.
	if v := os.Getenv(clientinfo.EnvKey); v != "" {
		req.Header.Set(clientinfo.Header, v)
	}

	if v := os.Getenv(FilesEnv); v != "" {
		req.Header.Set(FilesHeader, v)
	}
}
