// plugin_jobs.go tracks plugin installs that are still running.
//
// Installing means cloning a repository and running the plugin's lifecycle
// hooks, which is slow enough that holding the HTTP request open for it is not
// an option: a WebUI operator would sit on a spinner for a minute with no way
// to leave the page. So POST .../plugins/install starts the work in a goroutine
// and answers immediately.
//
// That immediately raises the question the async shape always raises: how does
// anyone learn it FAILED? A 202 that is never followed by anything means the
// card either turns installed or stays silent forever, and silence is the most
// common outcome of a bad repo, a missing hook, or a network that dropped.
//
// So the state lives here and is merged into the plugin LIST, rather than being
// handed out as a job id the caller has to remember. The list is what the UI is
// already polling, one card per plugin is already the shape, and a status that
// lives in the controller survives the operator reloading the page or opening
// it on their phone instead.
//
// In memory, deliberately. A controller restart loses "this failed 20 seconds
// ago", and that is the right trade: it is transient UI state, and a restart
// during an install is itself a failure the operator will read correctly as
// "it did not install". Nothing here is a source of truth — the disk is.
package controller

import (
	"sync"
	"time"
)

// Terminal entries are kept this long so a failure stays on screen long enough
// to be read, and a success stays until the next list confirms it from disk.
const pluginJobRetention = 10 * time.Minute

// PluginJobState is what a card renders as.
type PluginJobState string

const (
	PluginJobInstalling PluginJobState = "installing"
	PluginJobFailed     PluginJobState = "failed"
)

// PluginJob is one in-flight or recently-finished install.
//
// Name is best effort: until the clone lands and the manifest parses, the only
// handle we have is what the operator typed. The UI shows Source when Name is
// empty, which is why a failed install still produces a card the operator
// recognises instead of an anonymous error.
type PluginJob struct {
	Source    string         `json:"source"`
	Name      string         `json:"name,omitempty"`
	Version   string         `json:"version,omitempty"`
	State     PluginJobState `json:"state"`
	Error     string         `json:"error,omitempty"`
	StartedAt time.Time      `json:"started_at"`
	EndedAt   *time.Time     `json:"ended_at,omitempty"`
}

// pluginJobs is the per-process registry. Keyed by source rather than by name
// because the name is not known until the clone succeeds, and a failed install
// has to be addressable to be dismissed.
type pluginJobs struct {
	mu   sync.Mutex
	jobs map[string]*PluginJob
	now  func() time.Time // test seam
}

func newPluginJobs() *pluginJobs {
	return &pluginJobs{jobs: map[string]*PluginJob{}, now: time.Now}
}

// Begin records an install as started. It reports false when one is already
// running for the same source — a double-click, or an impatient operator on two
// tabs, must not start two clones racing to write the same directory.
func (p *pluginJobs) Begin(source string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	if existing, ok := p.jobs[source]; ok && existing.State == PluginJobInstalling {
		return false
	}

	p.jobs[source] = &PluginJob{
		Source:    source,
		State:     PluginJobInstalling,
		StartedAt: p.now(),
	}

	return true
}

// Succeed drops the entry: the plugin is on disk now, so the list reports it
// from the disk and a second source of truth would only be a way to disagree.
func (p *pluginJobs) Succeed(source string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	delete(p.jobs, source)
}

// Fail keeps the entry so the operator finds out. The message is whatever the
// installer said; the handler trims it before it gets here.
func (p *pluginJobs) Fail(source, message string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	ended := p.now()

	job, ok := p.jobs[source]
	if !ok {
		job = &PluginJob{Source: source, StartedAt: ended}
		p.jobs[source] = job
	}

	job.State = PluginJobFailed
	job.Error = message
	job.EndedAt = &ended
}

// Forget removes an entry by source.
func (p *pluginJobs) Forget(source string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	delete(p.jobs, source)
}

// ForgetByName removes entries whose plugin NAME matches, which is what an
// uninstall knows. Entries are keyed by source because the name does not exist
// until a clone succeeds — so an uninstall of "voodu-redis" has to find the job
// filed under "thadeu/voodu-redis" by deriving the name the same way the list
// does. Keying by source alone and deleting by name would silently miss, and
// the failed card would sit there implying the uninstall had not worked.
func (p *pluginJobs) ForgetByName(name string, nameOf func(string) string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for source, job := range p.jobs {
		known := job.Name
		if known == "" {
			known = nameOf(source)
		}

		if known == name {
			delete(p.jobs, source)
		}
	}
}

// List returns the current jobs, dropping terminal ones past their retention.
// Sweeping on read rather than on a timer keeps this free of a background
// goroutine whose only job is to delete a handful of map entries.
func (p *pluginJobs) List() []PluginJob {
	p.mu.Lock()
	defer p.mu.Unlock()

	cutoff := p.now().Add(-pluginJobRetention)
	out := make([]PluginJob, 0, len(p.jobs))

	for source, job := range p.jobs {
		if job.EndedAt != nil && job.EndedAt.Before(cutoff) {
			delete(p.jobs, source)

			continue
		}

		out = append(out, *job)
	}

	return out
}
