package remote

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseRemoteURL(t *testing.T) {
	ok := map[string]Info{
		"ubuntu@server:api":          {Host: "ubuntu@server", App: "api", BaseDir: "/opt/voodu"},
		"root@10.0.0.1:pg-0":         {Host: "root@10.0.0.1", App: "pg-0", BaseDir: "/opt/voodu"},
		"deploy@ec2.example.com:web": {Host: "deploy@ec2.example.com", App: "web", BaseDir: "/opt/voodu"},
	}

	for url, want := range ok {
		got, err := ParseRemoteURL(url)
		if err != nil {
			t.Errorf("%s: %v", url, err)
			continue
		}

		if got != want {
			t.Errorf("%s: got %+v, want %+v", url, got, want)
		}
	}

	bad := []string{
		"",
		"notarealurl",
		"ubuntu@server",               // no :app
		":api",                        // no host
		"ubuntu@server:",              // no app
		"https://github.com/foo/bar",  // git URL, not voodu
		"server:app",                  // missing user@
	}

	for _, url := range bad {
		if _, err := ParseRemoteURL(url); err == nil {
			t.Errorf("ParseRemoteURL(%q) should have failed", url)
		}
	}
}

func TestExtractFlags(t *testing.T) {
	cases := []struct {
		name       string
		in         []string
		wantRemote string
		wantApp    string
		wantRest   []string
	}{
		{
			"no flags",
			[]string{"config", "set", "FOO=bar"},
			"", "",
			[]string{"config", "set", "FOO=bar"},
		},
		{
			"app space separated",
			[]string{"config", "set", "FOO=bar", "-a", "api"},
			"", "api",
			[]string{"config", "set", "FOO=bar", "-a", "api"},
		},
		{
			"remote strip",
			[]string{"logs", "--remote", "prod", "-a", "web"},
			"prod", "web",
			[]string{"logs", "-a", "web"},
		},
		{
			"equals form",
			[]string{"postgres", "info", "--remote=staging", "--app=pg-0"},
			"staging", "pg-0",
			[]string{"postgres", "info", "--app=pg-0"},
		},
		{
			"long --app",
			[]string{"--app", "api", "status"},
			"", "api",
			[]string{"--app", "api", "status"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotR, gotA, gotRest := ExtractFlags(c.in)

			if gotR != c.wantRemote || gotA != c.wantApp {
				t.Errorf("flags: remote=%q app=%q (want %q/%q)", gotR, gotA, c.wantRemote, c.wantApp)
			}

			if !reflect.DeepEqual(gotRest, c.wantRest) {
				t.Errorf("rest: got %v, want %v", gotRest, c.wantRest)
			}
		})
	}
}

func TestWriteRCMode(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if CurrentMode() != ModeClient {
		t.Error("default mode should be client")
	}

	if err := WriteRCMode(ModeServer); err != nil {
		t.Fatal(err)
	}

	if CurrentMode() != ModeServer {
		t.Error("after WriteRCMode(server), expected server mode")
	}

	// Existing unrelated lines should survive a mode update.
	rc := filepath.Join(home, RCFileName)

	raw, _ := os.ReadFile(rc)
	if err := os.WriteFile(rc, append(raw, []byte("# comment\nunrelated=1\n")...), 0644); err != nil {
		t.Fatal(err)
	}

	if err := WriteRCMode(ModeClient); err != nil {
		t.Fatal(err)
	}

	raw, _ = os.ReadFile(rc)
	if !strings.Contains(string(raw), "unrelated=1") {
		t.Errorf("unrelated lines lost: %s", raw)
	}

	if CurrentMode() != ModeClient {
		t.Errorf("rewrite failed to update mode: %s", raw)
	}
}

func TestLookupAndResolve(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	tmp := t.TempDir()

	run := func(args ...string) {
		t.Helper()

		cmd := exec.Command("git", args...)
		cmd.Dir = tmp

		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}

	run("init", "-q")
	run("remote", "add", "voodu", "ubuntu@host1:api")
	run("remote", "add", "staging", "deploy@stage:api-stage")
	run("remote", "add", "github", "https://github.com/foo/bar.git") // should be ignored

	cwd, _ := os.Getwd()
	t.Chdir(tmp)

	t.Cleanup(func() { _ = os.Chdir(cwd) })

	// Direct lookup.
	info, err := Lookup("voodu")
	if err != nil || info.Host != "ubuntu@host1" || info.App != "api" {
		t.Errorf("Lookup voodu: %+v / %v", info, err)
	}

	// Missing remote.
	if _, err := Lookup("nope"); err == nil {
		t.Error("Lookup missing remote should fail")
	}

	// Resolve: --remote beats -a.
	info2, _ := Resolve("staging", "api")
	if info2 == nil || info2.RemoteName != "staging" {
		t.Errorf("Resolve --remote precedence: %+v", info2)
	}

	// Resolve: -a finds same-named remote.
	info3, _ := Resolve("", "staging")
	if info3 == nil || info3.RemoteName != "staging" {
		t.Errorf("Resolve -a match: %+v", info3)
	}

	// Resolve: -a falls back to default voodu remote, overrides App.
	info4, _ := Resolve("", "api-override")
	if info4 == nil || info4.RemoteName != "voodu" || info4.App != "api-override" {
		t.Errorf("Resolve -a fallback: %+v", info4)
	}

	// Resolve: nothing → default voodu.
	info5, _ := Resolve("", "")
	if info5 == nil || info5.RemoteName != "voodu" || info5.App != "api" {
		t.Errorf("Resolve default: %+v", info5)
	}

	// ListAll skips github (non-voodu URL).
	all, err := ListAll()
	if err != nil {
		t.Fatal(err)
	}

	names := map[string]bool{}
	for _, r := range all {
		names[r.RemoteName] = true
	}

	if !names["voodu"] || !names["staging"] || names["github"] {
		t.Errorf("ListAll: %+v", all)
	}
}
