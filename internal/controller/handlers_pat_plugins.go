// handlers_pat_plugins.go exposes plugin management on the PAT plane.
//
// The admin API already had these operations on 127.0.0.1:8686, which means
// they were reachable from the box and nowhere else — an operator wanting to
// install a plugin had to SSH in. These three routes are what let the dashboard
// do it instead.
//
// SCOPE, AND WHAT IT NOW MEANS. Listing is `read`. Install and uninstall are
// `actions`, the same scope that restarts a pod — and that is a deliberate
// widening the operator should know about, because installing is not like
// restarting. It clones a repository and runs that repository's lifecycle
// hooks as the controller's user. Anyone holding a PAT with `actions` can
// therefore run arbitrary code on this machine. Issue those tokens as if that
// is what they say, because it is.
package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.voodu.clowk.in/internal/plugins"
	"go.voodu.clowk.in/pkg/plugin"
)

// installTimeout bounds a background install. A clone that hangs on an
// unreachable host would otherwise hold its slot forever and the card would
// say "installing" until the process restarted.
const installTimeout = 10 * time.Minute

// installErrorLimit trims what reaches the operator. Hook output can be a
// build log; the last lines are the ones that say why.
const installErrorLimit = 600

func (a *API) pluginJobRegistry() *pluginJobs {
	a.pluginJobsOnce.Do(func() { a.pluginJobsReg = newPluginJobs() })

	return a.pluginJobsReg
}

// pluginListItem is one card. Installed plugins are read from disk; entries
// that are still installing (or that failed) come from the job registry, so a
// single list answers both "what is here" and "what is happening".
type pluginListItem struct {
	Name        string   `json:"name"`
	Version     string   `json:"version,omitempty"`
	Description string   `json:"description,omitempty"`
	Homepage    string   `json:"homepage,omitempty"`
	Aliases     []string `json:"aliases,omitempty"`
	Commands    []string `json:"commands,omitempty"`

	// State is "installed", "installing" or "failed".
	State  string `json:"state"`
	Source string `json:"source,omitempty"`
	Error  string `json:"error,omitempty"`
}

// handlePATPluginList reports every plugin on this controller plus every
// install still running or recently failed.
func (a *API) handlePATPluginList(w http.ResponseWriter, r *http.Request) {
	items := make([]pluginListItem, 0, 8)
	loadErrors := make([]string, 0)

	if a.PluginsRoot != "" {
		loaded, errs := plugins.LoadAll(a.PluginsRoot)
		for _, p := range loaded {
			items = append(items, installedItem(p.Manifest))
		}

		for _, e := range errs {
			loadErrors = append(loadErrors, e.Error())
		}
	}

	// Jobs last: a plugin being reinstalled is already on disk, and the
	// operator cares more that something is happening to it than that an
	// older copy is present. Matching by name lets the in-flight state win.
	for _, job := range a.pluginJobRegistry().List() {
		items = mergeJob(items, job)
	}

	writeJSON(w, http.StatusOK, envelope{
		Status: "ok",
		Data: map[string]any{
			"plugins": items,
			"errors":  loadErrors,
		},
	})
}

func installedItem(m plugin.Manifest) pluginListItem {
	commands := make([]string, 0, len(m.Commands))
	for _, c := range m.Commands {
		commands = append(commands, c.Name)
	}

	return pluginListItem{
		Name:        m.Name,
		Version:     m.Version,
		Description: m.Description,
		Homepage:    m.Homepage,
		Aliases:     m.Aliases,
		Commands:    commands,
		State:       "installed",
	}
}

// mergeJob folds an in-flight or failed install into the list. When the job is
// for a plugin already on disk it replaces that entry rather than adding a
// second card for the same thing; otherwise it appends, so a first install
// shows up the moment it starts instead of after it finishes.
//
// Matching on the derived name alone was not enough, and the failure was
// visible: `thadeu/voodu-traffik` derives "voodu-traffik" while the manifest
// inside it declares "traffik", so updating an installed plugin produced two
// cards — one installed, one installing — until the install finished and the
// job disappeared. A repository name is not a plugin name and never was.
//
// So the homepage is the second handle. An installed manifest points at where
// it came from, and comparing that to the source catches every plugin whose
// repository is named for the product rather than for the command.
func mergeJob(items []pluginListItem, job PluginJob) []pluginListItem {
	name := job.Name
	if name == "" {
		name = pluginNameFromSource(job.Source)
	}

	for i := range items {
		if items[i].Name != name && !sameRepo(items[i].Homepage, job.Source) {
			continue
		}

		items[i].State = string(job.State)
		items[i].Source = job.Source
		items[i].Error = job.Error

		return items
	}

	return append(items, pluginListItem{
		Name:   name,
		State:  string(job.State),
		Source: job.Source,
		Error:  job.Error,
	})
}

// sameRepo reports whether two references name the same repository.
// "https://github.com/thadeu/voodu-traffik" and "thadeu/voodu-traffik" do;
// so does a ".git" suffix or a trailing slash on either side. A local install
// path has no owner/repo shape and matches nothing, which is correct — two
// different directories are two different plugins.
func sameRepo(a, b string) bool {
	left, right := repoPath(a), repoPath(b)

	return left != "" && left == right
}

// repoPath reduces a reference to "owner/repo", lowercased. Anything that does
// not have that shape — a bare name, an absolute path — reduces to "" so it
// can never match something else by accident.
func repoPath(ref string) string {
	trimmed := strings.TrimSpace(ref)
	if trimmed == "" || strings.HasPrefix(trimmed, "/") {
		return ""
	}

	for _, prefix := range []string{"https://", "http://", "git@"} {
		trimmed = strings.TrimPrefix(trimmed, prefix)
	}

	trimmed = strings.TrimSuffix(strings.TrimRight(trimmed, "/"), ".git")
	trimmed = strings.ReplaceAll(trimmed, ":", "/")

	parts := strings.Split(trimmed, "/")
	if len(parts) < 2 {
		return ""
	}

	// The last two segments are owner and repo whether or not a host led.
	return strings.ToLower(parts[len(parts)-2] + "/" + parts[len(parts)-1])
}

// pluginNameFromSource guesses a display name before the manifest exists.
// "github.com/thadeu/voodu-redis" and "thadeu/voodu-redis" both become
// "voodu-redis" — good enough for a card that is about to be replaced by the
// real manifest, and far better than showing the operator a bare URL.
func pluginNameFromSource(source string) string {
	trimmed := strings.TrimSuffix(strings.TrimRight(source, "/"), ".git")

	if idx := strings.LastIndex(trimmed, "/"); idx >= 0 {
		trimmed = trimmed[idx+1:]
	}

	return trimmed
}

type pluginInstallRequest struct {
	Source  string `json:"source"`
	Version string `json:"version"`
}

// handlePATPluginInstall starts an install and returns immediately.
//
// 202, not 200: the work has been accepted, not done. The outcome arrives
// through the list — see plugin_jobs.go for why it is reported there rather
// than through a job id the caller would have to hold on to.
func (a *API) handlePATPluginInstall(w http.ResponseWriter, r *http.Request) {
	if a.PluginsRoot == "" {
		writeErr(w, http.StatusServiceUnavailable, fmt.Errorf("no plugin system on this controller"))

		return
	}

	var req pluginInstallRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid body: %w", err))

		return
	}

	req.Source = strings.TrimSpace(req.Source)
	req.Version = strings.TrimSpace(req.Version)

	// Rejected here rather than inside the goroutine: a source that cannot
	// be parsed is the operator's typo, and they should learn about it in
	// the response to their click, not as a card that fails a second later.
	if _, err := plugins.ParseSource(req.Source); err != nil {
		writeErr(w, http.StatusBadRequest, err)

		return
	}

	jobs := a.pluginJobRegistry()
	if !jobs.Begin(req.Source) {
		writeErr(w, http.StatusConflict, fmt.Errorf("an install of %s is already running", req.Source))

		return
	}

	// context.Background(), not r.Context(): the request is about to end,
	// and its cancellation would take the install down with it the instant
	// the operator's browser got the 202.
	go a.runPluginInstall(req.Source, req.Version)

	writeJSON(w, http.StatusAccepted, envelope{
		Status: "ok",
		Data: map[string]any{
			"source": req.Source,
			"state":  string(PluginJobInstalling),
		},
	})
}

func (a *API) runPluginInstall(source, version string) {
	ctx, cancel := context.WithTimeout(context.Background(), installTimeout)
	defer cancel()

	jobs := a.pluginJobRegistry()
	inst := &plugins.Installer{Root: a.PluginsRoot}

	if _, err := inst.Install(ctx, source, version); err != nil {
		jobs.Fail(source, trimInstallError(err.Error()))

		return
	}

	jobs.Succeed(source)
}

// trimInstallError keeps the tail. Hook failures print a build log and the
// reason is at the end of it; the head is almost always compiler noise.
func trimInstallError(msg string) string {
	msg = strings.TrimSpace(msg)
	if len(msg) <= installErrorLimit {
		return msg
	}

	return "…" + msg[len(msg)-installErrorLimit:]
}

// handlePATPluginRemove uninstalls, synchronously.
//
// Unlike install this is a directory removal plus an uninstall hook — fast
// enough to answer in the request, and the operator has just confirmed a
// destructive action, so an immediate answer is what the confirmation is for.
func (a *API) handlePATPluginRemove(w http.ResponseWriter, r *http.Request) {
	if a.PluginsRoot == "" {
		writeErr(w, http.StatusServiceUnavailable, fmt.Errorf("no plugin system on this controller"))

		return
	}

	name := r.PathValue("name")
	if strings.TrimSpace(name) == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("plugin name is required"))

		return
	}

	inst := &plugins.Installer{Root: a.PluginsRoot}

	ok, err := inst.Remove(r.Context(), name)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)

		return
	}

	if !ok {
		writeErr(w, http.StatusNotFound, fmt.Errorf("plugin %s is not installed", name))

		return
	}

	// A failed install for this plugin is no longer interesting once the
	// operator has removed it — leaving the card behind would make an
	// uninstall look like it had not worked.
	a.pluginJobRegistry().ForgetByName(name, pluginNameFromSource)

	writeJSON(w, http.StatusOK, envelope{Status: "ok", Data: map[string]any{"name": name}})
}
