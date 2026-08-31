package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writePlugin puts a minimal, loadable plugin on disk so LoadAll finds it.
func writePlugin(t *testing.T, root, name, version, desc, homepage string) {
	t.Helper()

	dir := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	manifest := "name: " + name + "\nversion: " + version +
		"\ndescription: " + desc + "\nhomepage: " + homepage + "\n"

	if err := os.WriteFile(filepath.Join(dir, "plugin.yml"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

func decodeList(t *testing.T, body []byte) []pluginListItem {
	t.Helper()

	var out struct {
		Data struct {
			Plugins []pluginListItem `json:"plugins"`
		} `json:"data"`
	}

	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}

	return out.Data.Plugins
}

func TestPluginListReportsInstalled(t *testing.T) {
	root := t.TempDir()
	writePlugin(t, root, "voodu-redis", "0.3.1", "Redis, declared like an app", "https://github.com/thadeu/voodu-redis")

	api := &API{Store: newMemStore(), PluginsRoot: root}
	rec := httptest.NewRecorder()

	api.handlePATPluginList(rec, httptest.NewRequest(http.MethodGet, "/api/pat/v1/plugins", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}

	items := decodeList(t, rec.Body.Bytes())
	if len(items) != 1 {
		t.Fatalf("want 1 plugin, got %d", len(items))
	}

	got := items[0]
	if got.Name != "voodu-redis" || got.Version != "0.3.1" || got.State != "installed" {
		t.Errorf("unexpected item: %+v", got)
	}

	// The card needs these to be a card at all, not a bare name.
	if got.Description == "" || got.Homepage == "" {
		t.Errorf("description/homepage missing: %+v", got)
	}
}

// An install in flight has to be visible BEFORE it finishes, or the operator
// clicks Install and the page looks like it ignored them.
func TestPluginListIncludesInFlightInstall(t *testing.T) {
	api := &API{Store: newMemStore(), PluginsRoot: t.TempDir()}
	api.pluginJobRegistry().Begin("thadeu/voodu-postgres")

	rec := httptest.NewRecorder()
	api.handlePATPluginList(rec, httptest.NewRequest(http.MethodGet, "/api/pat/v1/plugins", nil))

	items := decodeList(t, rec.Body.Bytes())
	if len(items) != 1 {
		t.Fatalf("want 1 item, got %d", len(items))
	}

	// Named from the source, because the manifest does not exist yet — a
	// card reading "thadeu/voodu-postgres" is worse than one reading
	// "voodu-postgres".
	if items[0].Name != "voodu-postgres" || items[0].State != "installing" {
		t.Errorf("unexpected item: %+v", items[0])
	}
}

// The whole reason the state lives in the list: a failure has to reach the
// operator, and it has to say why.
func TestPluginListReportsFailure(t *testing.T) {
	api := &API{Store: newMemStore(), PluginsRoot: t.TempDir()}
	api.pluginJobRegistry().Begin("thadeu/nope")
	api.pluginJobRegistry().Fail("thadeu/nope", "git clone failed: repository not found")

	rec := httptest.NewRecorder()
	api.handlePATPluginList(rec, httptest.NewRequest(http.MethodGet, "/api/pat/v1/plugins", nil))

	items := decodeList(t, rec.Body.Bytes())
	if len(items) != 1 || items[0].State != "failed" {
		t.Fatalf("unexpected items: %+v", items)
	}

	if !strings.Contains(items[0].Error, "repository not found") {
		t.Errorf("error not carried through: %q", items[0].Error)
	}
}

// A reinstall must not produce two cards for one plugin.
func TestPluginListMergesJobOntoInstalledPlugin(t *testing.T) {
	root := t.TempDir()
	writePlugin(t, root, "voodu-redis", "0.3.1", "d", "h")

	api := &API{Store: newMemStore(), PluginsRoot: root}
	api.pluginJobRegistry().Begin("thadeu/voodu-redis")

	rec := httptest.NewRecorder()
	api.handlePATPluginList(rec, httptest.NewRequest(http.MethodGet, "/api/pat/v1/plugins", nil))

	items := decodeList(t, rec.Body.Bytes())
	if len(items) != 1 {
		t.Fatalf("want the job merged onto the installed entry, got %d cards: %+v", len(items), items)
	}

	if items[0].State != "installing" {
		t.Errorf("in-flight state should win over on-disk: %+v", items[0])
	}
}

func TestPluginInstallRejectsAnUnparseableSource(t *testing.T) {
	api := &API{Store: newMemStore(), PluginsRoot: t.TempDir()}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/pat/v1/plugins/install",
		strings.NewReader(`{"source":""}`))

	api.handlePATPluginInstall(rec, req)

	// The operator's typo comes back in the answer to their click, not as a
	// card that fails a second later.
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: %d, want 400", rec.Code)
	}

	if len(api.pluginJobRegistry().List()) != 0 {
		t.Errorf("a rejected source must not leave a job behind")
	}
}

// A double-click, or two tabs, must not start two clones racing to write the
// same directory.
func TestPluginInstallRefusesADuplicate(t *testing.T) {
	api := &API{Store: newMemStore(), PluginsRoot: t.TempDir()}
	api.pluginJobRegistry().Begin("thadeu/voodu-redis")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/pat/v1/plugins/install",
		strings.NewReader(`{"source":"thadeu/voodu-redis"}`))

	api.handlePATPluginInstall(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status: %d, want 409", rec.Code)
	}
}

func TestPluginRemoveIsNotFoundWhenAbsent(t *testing.T) {
	api := &API{Store: newMemStore(), PluginsRoot: t.TempDir()}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/pat/v1/plugins/ghost", nil)
	req.SetPathValue("name", "ghost")

	api.handlePATPluginRemove(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: %d, want 404", rec.Code)
	}
}

// The bug this pins: jobs are keyed by SOURCE ("thadeu/voodu-redis") and an
// uninstall only knows the NAME ("voodu-redis"). Deleting by name would miss,
// and the failed card would sit there implying the uninstall had not worked.
func TestRemoveClearsAFailedInstallForTheSamePlugin(t *testing.T) {
	root := t.TempDir()
	writePlugin(t, root, "voodu-redis", "0.3.1", "d", "h")

	api := &API{Store: newMemStore(), PluginsRoot: root}
	api.pluginJobRegistry().Begin("thadeu/voodu-redis")
	api.pluginJobRegistry().Fail("thadeu/voodu-redis", "hook exited 1")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/pat/v1/plugins/voodu-redis", nil)
	req.SetPathValue("name", "voodu-redis")

	api.handlePATPluginRemove(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", rec.Code, rec.Body.String())
	}

	if jobs := api.pluginJobRegistry().List(); len(jobs) != 0 {
		t.Errorf("the failed job outlived the uninstall: %+v", jobs)
	}
}

// Terminal entries are swept on read so a failure does not haunt the screen
// forever, and so nothing needs a goroutine whose only job is deleting keys.
func TestFailedJobsExpire(t *testing.T) {
	jobs := newPluginJobs()
	now := time.Now()
	jobs.now = func() time.Time { return now }

	jobs.Begin("a/b")
	jobs.Fail("a/b", "boom")

	if len(jobs.List()) != 1 {
		t.Fatalf("a fresh failure should be listed")
	}

	now = now.Add(pluginJobRetention + time.Minute)

	if got := jobs.List(); len(got) != 0 {
		t.Errorf("an expired failure should be swept: %+v", got)
	}
}

// An install still running is NOT swept, however long it takes — a slow clone
// is not a stale entry, and dropping it would make the card vanish mid-install.
func TestRunningJobsAreNeverSwept(t *testing.T) {
	jobs := newPluginJobs()
	now := time.Now()
	jobs.now = func() time.Time { return now }

	jobs.Begin("a/b")
	now = now.Add(10 * pluginJobRetention)

	if len(jobs.List()) != 1 {
		t.Errorf("a running install must not be swept")
	}
}

func TestSuccessDropsTheJob(t *testing.T) {
	jobs := newPluginJobs()
	jobs.Begin("a/b")
	jobs.Succeed("a/b")

	// Once it is on disk, the disk is the source of truth. A second one
	// here could only ever disagree with it.
	if got := jobs.List(); len(got) != 0 {
		t.Errorf("a successful install should leave no job: %+v", got)
	}
}

func TestPluginNameFromSource(t *testing.T) {
	cases := map[string]string{
		"thadeu/voodu-redis":                        "voodu-redis",
		"github.com/thadeu/voodu-redis":             "voodu-redis",
		"https://github.com/thadeu/voodu-redis.git": "voodu-redis",
		"/opt/local/voodu-caddy":                    "voodu-caddy",
		"voodu-postgres":                            "voodu-postgres",
	}

	for source, want := range cases {
		if got := pluginNameFromSource(source); got != want {
			t.Errorf("%q → %q, want %q", source, got, want)
		}
	}
}

// The duplicate-card bug, exactly as it appeared on screen: the repository is
// voodu-traffik and the plugin inside it is called traffik, so matching on the
// derived name alone produced two cards for one plugin while it updated.
func TestUpdatingAPluginWhoseRepoIsNamedDifferently(t *testing.T) {
	root := t.TempDir()
	writePlugin(t, root, "traffik", "0.1.1", "layer 4", "https://github.com/thadeu/voodu-traffik")

	api := &API{Store: newMemStore(), PluginsRoot: root}
	api.pluginJobRegistry().Begin("thadeu/voodu-traffik")

	rec := httptest.NewRecorder()
	api.handlePATPluginList(rec, httptest.NewRequest(http.MethodGet, "/api/pat/v1/plugins", nil))

	items := decodeList(t, rec.Body.Bytes())
	if len(items) != 1 {
		t.Fatalf("one plugin should be one card, got %d: %+v", len(items), items)
	}

	if items[0].Name != "traffik" || items[0].State != "installing" {
		t.Errorf("unexpected card: %+v", items[0])
	}
}

func TestSameRepo(t *testing.T) {
	same := [][2]string{
		{"https://github.com/thadeu/voodu-traffik", "thadeu/voodu-traffik"},
		{"https://github.com/thadeu/voodu-traffik.git", "github.com/thadeu/voodu-traffik"},
		{"https://github.com/Thadeu/Voodu-Redis", "thadeu/voodu-redis"},
		{"https://github.com/thadeu/voodu-redis/", "git@github.com:thadeu/voodu-redis.git"},
	}

	for _, pair := range same {
		if !sameRepo(pair[0], pair[1]) {
			t.Errorf("%q and %q should be the same repo", pair[0], pair[1])
		}
	}

	differ := [][2]string{
		{"https://github.com/thadeu/voodu-redis", "thadeu/voodu-postgres"},
		{"https://github.com/other/voodu-redis", "thadeu/voodu-redis"},
		// A local path is not a repository, and two directories that happen
		// to end in the same name are still two different plugins.
		{"/opt/local/voodu-redis", "/opt/other/voodu-redis"},
		{"", ""},
	}

	for _, pair := range differ {
		if sameRepo(pair[0], pair[1]) {
			t.Errorf("%q and %q should NOT match", pair[0], pair[1])
		}
	}
}
