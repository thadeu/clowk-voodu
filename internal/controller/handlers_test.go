package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"reflect"
	"sort"
	"strings"
	"testing"

	"go.voodu.clowk.in/internal/plugins"
	"go.voodu.clowk.in/pkg/plugin"
)

// fakeInvoker records every Invoke call and returns scripted results so
// tests can assert that the handler invoked the correct plugin command
// with the right env vars.
type fakeInvoker struct {
	calls   []invokerCall
	results map[string]*plugins.Result // keyed by "plugin.command"
}

type invokerCall struct {
	Plugin  string
	Command string
	Args    []string
	Env     map[string]string
}

func (f *fakeInvoker) Invoke(_ context.Context, pluginName, command string, args []string, env map[string]string) (*plugins.Result, error) {
	f.calls = append(f.calls, invokerCall{
		Plugin:  pluginName,
		Command: command,
		Args:    append([]string(nil), args...),
		Env:     copyMap(env),
	})

	key := pluginName + "." + command

	if r, ok := f.results[key]; ok {
		return r, nil
	}

	return &plugins.Result{ExitCode: 0}, nil
}

func copyMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// envelopeResult is a tiny helper to build a *plugins.Result carrying
// the JSON envelope a database plugin would actually produce.
func envelopeResult(data map[string]any) *plugins.Result {
	env := &plugin.Envelope{Status: "ok", Data: data}
	raw, _ := json.Marshal(env)
	return &plugins.Result{Raw: raw, Envelope: env}
}

func TestDatabaseHandler_CreatePersistsEnvelope(t *testing.T) {
	store := newMemStore()
	inv := &fakeInvoker{
		results: map[string]*plugins.Result{
			"postgres.create": envelopeResult(map[string]any{
				"url":  "postgres://u:p@h:5432/db",
				"host": "h",
			}),
		},
	}

	h := &DatabaseHandler{Store: store, Invoker: inv, Log: quietLogger()}

	spec := databaseSpec{Engine: "postgres", Version: "16"}
	ev := putEvent(t, KindDatabase, "main", spec)

	h.Handle(context.Background(), ev)

	if len(inv.calls) != 1 {
		t.Fatalf("expected 1 invoker call, got %d", len(inv.calls))
	}

	call := inv.calls[0]

	if call.Plugin != "postgres" || call.Command != "create" {
		t.Errorf("bad plugin/command: %s/%s", call.Plugin, call.Command)
	}

	if call.Env[plugin.EnvDBEngine] != "postgres" || call.Env[plugin.EnvDBVersion] != "16" {
		t.Errorf("env missing spec fields: %+v", call.Env)
	}

	// Status should carry both engine and the envelope data.
	raw, _ := store.GetStatus(context.Background(), KindDatabase, "main")
	if raw == nil {
		t.Fatal("no status persisted")
	}

	var status DatabaseStatus
	if err := json.Unmarshal(raw, &status); err != nil {
		t.Fatal(err)
	}

	if status.Engine != "postgres" {
		t.Errorf("engine: %q", status.Engine)
	}

	if status.Data["url"] != "postgres://u:p@h:5432/db" {
		t.Errorf("url field missing from status data: %+v", status.Data)
	}
}

func TestDatabaseHandler_IdempotentOnReplay(t *testing.T) {
	store := newMemStore()
	inv := &fakeInvoker{
		results: map[string]*plugins.Result{
			"postgres.create": envelopeResult(map[string]any{"url": "x"}),
		},
	}

	h := &DatabaseHandler{Store: store, Invoker: inv, Log: quietLogger()}

	ev := putEvent(t, KindDatabase, "main", databaseSpec{Engine: "postgres"})

	h.Handle(context.Background(), ev) // first call → invoke plugin
	h.Handle(context.Background(), ev) // replay → must NOT re-invoke

	if len(inv.calls) != 1 {
		t.Errorf("expected 1 invoke on replay, got %d", len(inv.calls))
	}
}

func TestDatabaseHandler_DestroyCallsPluginAndClearsStatus(t *testing.T) {
	store := newMemStore()

	// Seed status as if a previous create had succeeded.
	pre, _ := json.Marshal(DatabaseStatus{Engine: "postgres", Data: map[string]any{"url": "x"}})
	_ = store.PutStatus(context.Background(), KindDatabase, "main", pre)

	inv := &fakeInvoker{}

	h := &DatabaseHandler{Store: store, Invoker: inv, Log: quietLogger()}

	h.Handle(context.Background(), WatchEvent{Type: WatchDelete, Kind: KindDatabase, Name: "main"})

	if len(inv.calls) != 1 || inv.calls[0].Command != "destroy" {
		t.Fatalf("destroy not called: %+v", inv.calls)
	}

	raw, _ := store.GetStatus(context.Background(), KindDatabase, "main")
	if raw != nil {
		t.Errorf("status not cleared after destroy")
	}
}

func TestDeploymentHandler_ResolvesRefsIntoEnv(t *testing.T) {
	store := newMemStore()

	// Pretend a postgres DB was reconciled earlier.
	ds, _ := json.Marshal(DatabaseStatus{
		Engine: "postgres",
		Data: map[string]any{
			"url":  "postgres://u:p@h:5432/db",
			"host": "h",
		},
	})
	_ = store.PutStatus(context.Background(), KindDatabase, "main", ds)

	var writes []envWrite

	h := &DeploymentHandler{
		Store: store,
		Log:   quietLogger(),
		WriteEnv: func(app string, pairs []string) (bool, error) {
			writes = append(writes, envWrite{App: app, Pairs: append([]string(nil), pairs...)})
			return true, nil
		},
	}

	spec := deploymentSpec{
		Env: map[string]string{
			"DATABASE_URL": "${ref.database.main.url}",
			"DB_HOST":      "${ref.database.main.host}",
			"STATIC":       "plain",
		},
	}

	ev := putEvent(t, KindDeployment, "api", spec)

	h.Handle(context.Background(), ev)

	if len(writes) != 1 {
		t.Fatalf("expected 1 env write, got %d", len(writes))
	}

	if writes[0].App != "api" {
		t.Errorf("wrong app: %q", writes[0].App)
	}

	want := []string{
		"DATABASE_URL=postgres://u:p@h:5432/db",
		"DB_HOST=h",
		"STATIC=plain",
	}

	sort.Strings(writes[0].Pairs)

	if !reflect.DeepEqual(writes[0].Pairs, want) {
		t.Errorf("pairs:\n  got:  %v\n  want: %v", writes[0].Pairs, want)
	}
}

func TestDeploymentHandler_UnresolvedRefIsTransient(t *testing.T) {
	store := newMemStore()

	var writes []envWrite

	h := &DeploymentHandler{
		Store: store,
		Log:   quietLogger(),
		WriteEnv: func(app string, pairs []string) (bool, error) {
			writes = append(writes, envWrite{App: app, Pairs: pairs})
			return true, nil
		},
	}

	spec := deploymentSpec{
		Env: map[string]string{"URL": "${ref.database.ghost.url}"},
	}

	ev := putEvent(t, KindDeployment, "api", spec)

	err := h.Handle(context.Background(), ev)
	if err == nil {
		t.Fatal("expected error for unresolved ref")
	}

	if !isTransient(err) {
		t.Errorf("unresolved ref should be transient, got %T: %v", err, err)
	}

	if !strings.Contains(err.Error(), "database.ghost.url") {
		t.Errorf("error should name the missing ref, got: %v", err)
	}

	if len(writes) != 0 {
		t.Errorf("WriteEnv should not be called when a ref is missing")
	}
}

type envWrite struct {
	App   string
	Pairs []string
}

// fakeContainers records Ensure/Restart/Recreate calls and lets tests
// pretend a container already exists (+ a current image tag) so we can
// assert idempotent reconcile and drift detection.
type fakeContainers struct {
	exists    map[string]bool
	images    map[string]string
	ensures   []ContainerSpec
	restarts  []string
	recreates []ContainerSpec
	removes   []string

	// containerImageIDs maps container name → the image ID the container
	// was frozen against at create time. tagImageIDs maps tag → the image
	// ID that tag currently resolves to. Together they simulate the
	// docker invariant the real ImageIDsDiffer relies on: a container
	// keeps its original ID even if the tag is rebuilt under it.
	containerImageIDs map[string]string
	tagImageIDs       map[string]string
}

func (f *fakeContainers) Exists(name string) (bool, error) { return f.exists[name], nil }

func (f *fakeContainers) Image(name string) (string, error) {
	return f.images[name], nil
}

func (f *fakeContainers) ImageIDsDiffer(container, tag string) (bool, error) {
	cid := f.containerImageIDs[container]
	tid := f.tagImageIDs[tag]

	// Match production's "unknown → no drift" contract: if we can't
	// resolve either side, fall back to the spec-hash path.
	if cid == "" || tid == "" {
		return false, nil
	}

	return cid != tid, nil
}

func (f *fakeContainers) Recreate(spec ContainerSpec) error {
	f.recreates = append(f.recreates, spec)

	if f.images == nil {
		f.images = map[string]string{}
	}

	f.images[spec.Name] = spec.Image

	return nil
}

func (f *fakeContainers) Ensure(spec ContainerSpec) (bool, error) {
	f.ensures = append(f.ensures, spec)

	if f.exists == nil {
		f.exists = map[string]bool{}
	}

	// Mirror the production contract: Ensure reports true only when it
	// actually created a container. Tests that pre-populate exists get
	// a "no-op" return, which is what the restart branch keys off.
	if f.exists[spec.Name] {
		return false, nil
	}

	f.exists[spec.Name] = true

	return true, nil
}

func (f *fakeContainers) Restart(name string) error {
	f.restarts = append(f.restarts, name)
	return nil
}

func (f *fakeContainers) Remove(name string) error {
	f.removes = append(f.removes, name)

	if f.exists != nil {
		delete(f.exists, name)
	}

	if f.images != nil {
		delete(f.images, name)
	}

	return nil
}

func (f *fakeContainers) ListByAppPrefix(app string) ([]string, error) {
	out := []string{}

	for name, present := range f.exists {
		if !present {
			continue
		}

		if name == app {
			out = append(out, name)
			continue
		}

		rest, ok := strings.CutPrefix(name, app+"-")
		if !ok {
			continue
		}

		allDigits := rest != ""
		for _, r := range rest {
			if r < '0' || r > '9' {
				allDigits = false
				break
			}
		}

		if allDigits {
			out = append(out, name)
		}
	}

	sort.Strings(out)

	return out, nil
}

func TestDeploymentHandler_SpawnsContainerWhenImageSet(t *testing.T) {
	store := newMemStore()

	var writes []envWrite

	cm := &fakeContainers{}

	h := &DeploymentHandler{
		Store:       store,
		Log:         quietLogger(),
		WriteEnv: func(app string, pairs []string) (bool, error) {
			writes = append(writes, envWrite{app, append([]string(nil), pairs...)})
			return true, nil
		},
		EnvFilePath: func(app string) string { return "/tmp/" + app + ".env" },
		Containers:  cm,
	}

	spec := deploymentSpec{
		Image:    "ghcr.io/acme/api:1.2.3",
		Command:  []string{"serve"},
		Env:      map[string]string{"FOO": "bar"},
		Ports:    []string{"8080:8080"},
		Networks: []string{"voodu0"},
		Restart:  "unless-stopped",
	}

	ev := putEvent(t, KindDeployment, "api", spec)

	h.Handle(context.Background(), ev)

	if len(cm.ensures) != 1 {
		t.Fatalf("expected 1 ensure, got %d", len(cm.ensures))
	}

	got := cm.ensures[0]
	if got.Name != "api-0" || got.Image != "ghcr.io/acme/api:1.2.3" || got.EnvFile != "/tmp/api.env" {
		t.Errorf("unexpected ensure spec: %+v", got)
	}

	if got.Restart != "unless-stopped" || len(got.Networks) != 1 || got.Networks[0] != "voodu0" {
		t.Errorf("runtime flags not forwarded: %+v", got)
	}

	// Env write still happens alongside the container ensure.
	if len(writes) != 1 || writes[0].Pairs[0] != "FOO=bar" {
		t.Errorf("env write missing or wrong: %+v", writes)
	}
}

func TestDeploymentHandler_AlwaysJoinsVoodu0(t *testing.T) {
	// voodu0 is the platform's plumbing bridge — caddy and plugins live
	// there, so the handler MUST ensure every container joins it, even
	// when the operator declares a different primary network. Without
	// this invariant, `networks = ["db"]` would produce an app that's
	// invisible to ingress, which is almost never what the operator meant.
	cases := []struct {
		name string
		spec deploymentSpec
		want []string
	}{
		{
			name: "omitted → voodu0",
			spec: deploymentSpec{Image: "img:1"},
			want: []string{"voodu0"},
		},
		{
			name: "legacy singular network → [network, voodu0]",
			spec: deploymentSpec{Image: "img:1", Network: "db"},
			want: []string{"db", "voodu0"},
		},
		{
			name: "explicit networks → voodu0 appended",
			spec: deploymentSpec{Image: "img:1", Networks: []string{"db"}},
			want: []string{"db", "voodu0"},
		},
		{
			name: "operator already included voodu0 → deduped, order preserved",
			spec: deploymentSpec{Image: "img:1", Networks: []string{"voodu0", "db"}},
			want: []string{"voodu0", "db"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cm := &fakeContainers{}

			h := &DeploymentHandler{
				Store:       newMemStore(),
				Log:         quietLogger(),
				WriteEnv:    func(string, []string) (bool, error) { return false, nil },
				EnvFilePath: func(app string) string { return "/tmp/" + app + ".env" },
				Containers:  cm,
			}

			ev := putEvent(t, KindDeployment, "app", tc.spec)
			h.Handle(context.Background(), ev)

			if len(cm.ensures) != 1 {
				t.Fatalf("expected 1 ensure, got %d", len(cm.ensures))
			}

			got := cm.ensures[0].Networks
			if len(got) != len(tc.want) {
				t.Fatalf("networks mismatch: want %v, got %v", tc.want, got)
			}

			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("networks[%d]: want %q, got %q (full: %v)", i, tc.want[i], got[i], got)
				}
			}
		})
	}
}

func TestNormalizePorts(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"bare container port gets loopback + random host", "80", "127.0.0.1::80"},
		{"bare port with proto", "53/udp", "127.0.0.1::53/udp"},
		{"host:container gets loopback prefix", "3000:80", "127.0.0.1:3000:80"},
		{"host:container with proto", "3000:80/udp", "127.0.0.1:3000:80/udp"},
		{"explicit 127.0.0.1 passes through", "127.0.0.1:3000:80", "127.0.0.1:3000:80"},
		{"explicit 0.0.0.0 = operator opted into exposure", "0.0.0.0:5432:5432", "0.0.0.0:5432:5432"},
		{"pinned interface IP passes through", "192.168.1.5:3000:80", "192.168.1.5:3000:80"},
		{"ipv6 literal passes through", "[::1]:3000:80", "[::1]:3000:80"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizePort(tc.in); got != tc.want {
				t.Errorf("normalizePort(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestDeploymentHandler_DefaultsToLoopbackPorts(t *testing.T) {
	// End-to-end: handler normalizes Ports before handing them to the
	// container manager, so a naive `ports = ["3000:80"]` manifest
	// never produces a world-exposed container.
	cm := &fakeContainers{}

	h := &DeploymentHandler{
		Store:       newMemStore(),
		Log:         quietLogger(),
		WriteEnv:    func(string, []string) (bool, error) { return false, nil },
		EnvFilePath: func(app string) string { return "/tmp/" + app + ".env" },
		Containers:  cm,
	}

	ev := putEvent(t, KindDeployment, "web", deploymentSpec{
		Image: "img:1",
		Ports: []string{"3000:80", "0.0.0.0:5432:5432"},
	})

	h.Handle(context.Background(), ev)

	got := cm.ensures[0].Ports
	if len(got) != 2 {
		t.Fatalf("ports: want 2, got %d (%+v)", len(got), got)
	}

	if got[0] != "127.0.0.1:3000:80" {
		t.Errorf("default port should be loopback-bound, got %q", got[0])
	}

	if got[1] != "0.0.0.0:5432:5432" {
		t.Errorf("explicit 0.0.0.0 must pass through verbatim, got %q", got[1])
	}
}

func TestDeploymentHandler_HostNetworkMode(t *testing.T) {
	// network_mode = "host" is the escape hatch for apps that need the
	// host's net stack directly (WebRTC/SIP/RTP/raw sockets). It's
	// mutually exclusive with bridges — no voodu0 auto-append, no
	// `networks = [...]` join. The handler forwards NetworkMode and
	// leaves Networks empty so the docker layer uses `--network host`.
	cm := &fakeContainers{}

	h := &DeploymentHandler{
		Store:       newMemStore(),
		Log:         quietLogger(),
		WriteEnv:    func(string, []string) (bool, error) { return false, nil },
		EnvFilePath: func(app string) string { return "/tmp/" + app + ".env" },
		Containers:  cm,
	}

	ev := putEvent(t, KindDeployment, "sip", deploymentSpec{
		Image:       "sip-gw:1",
		NetworkMode: "host",
	})

	if err := h.Handle(context.Background(), ev); err != nil {
		t.Fatalf("host mode should be accepted: %v", err)
	}

	if len(cm.ensures) != 1 {
		t.Fatalf("expected 1 ensure, got %d", len(cm.ensures))
	}

	got := cm.ensures[0]
	if got.NetworkMode != "host" {
		t.Errorf("NetworkMode not forwarded: want \"host\", got %q", got.NetworkMode)
	}

	if len(got.Networks) != 0 {
		t.Errorf("host mode must skip bridge networks, got %v", got.Networks)
	}
}

func TestDeploymentHandler_NetworkModeExclusivity(t *testing.T) {
	// host/none + networks must error loud — silently dropping one side
	// produces surprising runtime behaviour.
	cases := []struct {
		name string
		spec deploymentSpec
	}{
		{"host + networks", deploymentSpec{Image: "x:1", NetworkMode: "host", Networks: []string{"db"}}},
		{"host + network",  deploymentSpec{Image: "x:1", NetworkMode: "host", Network:  "db"}},
		{"none + networks", deploymentSpec{Image: "x:1", NetworkMode: "none", Networks: []string{"voodu0"}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &DeploymentHandler{
				Store:       newMemStore(),
				Log:         quietLogger(),
				WriteEnv:    func(string, []string) (bool, error) { return false, nil },
				EnvFilePath: func(app string) string { return "/tmp/" + app + ".env" },
				Containers:  &fakeContainers{},
			}

			ev := putEvent(t, KindDeployment, "app", tc.spec)
			if err := h.Handle(context.Background(), ev); err == nil {
				t.Errorf("expected error for %s, got nil", tc.name)
			}
		})
	}
}

func TestDeploymentHandler_UnknownNetworkModeRejected(t *testing.T) {
	// Accept only "host" / "none" explicitly. "bridge" = omit; anything
	// else is a typo we want to flag early instead of passing through
	// to docker where the error message is worse.
	h := &DeploymentHandler{
		Store:       newMemStore(),
		Log:         quietLogger(),
		WriteEnv:    func(string, []string) (bool, error) { return false, nil },
		EnvFilePath: func(app string) string { return "/tmp/" + app + ".env" },
		Containers:  &fakeContainers{},
	}

	ev := putEvent(t, KindDeployment, "app", deploymentSpec{
		Image:       "x:1",
		NetworkMode: "bridge",
	})

	if err := h.Handle(context.Background(), ev); err == nil {
		t.Errorf("expected rejection of network_mode=\"bridge\"")
	}
}

func TestDeploymentHandler_EnsureIsIdempotent(t *testing.T) {
	store := newMemStore()

	cm := &fakeContainers{}

	h := &DeploymentHandler{
		Store:       store,
		Log:         quietLogger(),
		WriteEnv:    func(string, []string) (bool, error) { return false, nil },
		EnvFilePath: func(app string) string { return "/tmp/" + app + ".env" },
		Containers:  cm,
	}

	ev := putEvent(t, KindDeployment, "api", deploymentSpec{
		Image: "img:1",
		Env:   map[string]string{"X": "1"},
	})

	h.Handle(context.Background(), ev)
	h.Handle(context.Background(), ev) // replay

	// First call creates; second call hits the exists-short-circuit and
	// returns created=false. Both calls still hit the fake's Ensure
	// slice — the slice is the contract surface tests rely on.
	if len(cm.ensures) != 2 {
		t.Fatalf("expected 2 ensure calls on replay, got %d", len(cm.ensures))
	}

	if cm.ensures[0].Image != cm.ensures[1].Image {
		t.Errorf("spec drifted between replays: %+v vs %+v", cm.ensures[0], cm.ensures[1])
	}

	// Critically: env did not change (WriteEnv reports false), so we
	// must NOT restart. A restart on every replay is the exact churn
	// this gap is preventing.
	if len(cm.restarts) != 0 {
		t.Errorf("replay with unchanged env must not restart, got %+v", cm.restarts)
	}
}

func TestDeploymentHandler_RestartsWhenEnvChangedAndContainerExists(t *testing.T) {
	store := newMemStore()

	cm := &fakeContainers{
		// Pretend the indexed slot already exists (e.g. a previous
		// reconcile) so Ensure returns created=false and the restart
		// branch fires for an env change.
		exists: map[string]bool{"api-0": true},
	}

	h := &DeploymentHandler{
		Store:       store,
		Log:         quietLogger(),
		WriteEnv:    func(string, []string) (bool, error) { return true, nil }, // changed
		EnvFilePath: func(app string) string { return "/tmp/" + app + ".env" },
		Containers:  cm,
	}

	ev := putEvent(t, KindDeployment, "api", deploymentSpec{
		Image: "img:1",
		Env:   map[string]string{"X": "new"},
	})

	h.Handle(context.Background(), ev)

	if got, want := cm.restarts, []string{"api-0"}; len(got) != len(want) || got[0] != want[0] {
		t.Errorf("expected restart of api-0, got %+v", got)
	}
}

func TestDeploymentHandler_RecreatesOnImageDrift(t *testing.T) {
	store := newMemStore()

	// Pre-seed status as if a prior reconcile had run with v1.0.0.
	// Without this, the no-status baseline path would mask the drift
	// on first reconcile after upgrade — by design.
	prevHash := deploymentSpecHash(deploymentSpec{Image: "ghcr.io/acme/api:1.0.0"})
	pre, _ := json.Marshal(DeploymentStatus{Image: "ghcr.io/acme/api:1.0.0", SpecHash: prevHash})
	_ = store.PutStatus(context.Background(), KindDeployment, "api", pre)

	cm := &fakeContainers{
		exists: map[string]bool{"api-0": true},
		images: map[string]string{"api-0": "ghcr.io/acme/api:1.0.0"},
	}

	h := &DeploymentHandler{
		Store:       store,
		Log:         quietLogger(),
		WriteEnv:    func(string, []string) (bool, error) { return false, nil },
		EnvFilePath: func(app string) string { return "/tmp/" + app + ".env" },
		Containers:  cm,
	}

	// New manifest bumps image version.
	ev := putEvent(t, KindDeployment, "api", deploymentSpec{
		Image: "ghcr.io/acme/api:2.0.0",
		Env:   map[string]string{"X": "1"},
	})

	h.Handle(context.Background(), ev)

	if len(cm.recreates) != 1 {
		t.Fatalf("expected 1 recreate on image drift, got %d", len(cm.recreates))
	}

	if cm.recreates[0].Image != "ghcr.io/acme/api:2.0.0" {
		t.Errorf("recreate spec image: got %q", cm.recreates[0].Image)
	}

	// Recreate absorbs env pickup — a trailing restart would stop the
	// just-created container for nothing.
	if len(cm.restarts) != 0 {
		t.Errorf("recreate path must not trigger restart, got %+v", cm.restarts)
	}

	// Status must be re-baselined to the new image so subsequent
	// replays with the same manifest don't trigger another recreate.
	raw, _ := store.GetStatus(context.Background(), KindDeployment, "api")

	var st DeploymentStatus
	_ = json.Unmarshal(raw, &st)

	if st.Image != "ghcr.io/acme/api:2.0.0" {
		t.Errorf("status image not updated post-recreate: %+v", st)
	}
}

func TestDeploymentHandler_RecreatesOnPortsDrift(t *testing.T) {
	store := newMemStore()

	// Prior reconcile ran with no port bindings.
	prevSpec := deploymentSpec{Image: "nginx:latest", Ports: []string{"80"}}
	prevHash := deploymentSpecHash(prevSpec)
	pre, _ := json.Marshal(DeploymentStatus{Image: prevSpec.Image, SpecHash: prevHash})
	_ = store.PutStatus(context.Background(), KindDeployment, "web", pre)

	cm := &fakeContainers{
		exists: map[string]bool{"web-0": true},
		images: map[string]string{"web-0": "nginx:latest"},
	}

	h := &DeploymentHandler{
		Store:       store,
		Log:         quietLogger(),
		WriteEnv:    func(string, []string) (bool, error) { return false, nil },
		EnvFilePath: func(app string) string { return "/tmp/" + app + ".env" },
		Containers:  cm,
	}

	// Same image, but ports change — should recreate.
	ev := putEvent(t, KindDeployment, "web", deploymentSpec{
		Image: "nginx:latest",
		Ports: []string{"80:80"},
	})

	h.Handle(context.Background(), ev)

	if len(cm.recreates) != 1 {
		t.Fatalf("expected 1 recreate on ports drift, got %d", len(cm.recreates))
	}

	// Port is normalized to loopback-bound by the handler's private-by-
	// default policy — raw "80:80" becomes "127.0.0.1:80:80" in the
	// spec that reaches the container manager.
	if got := cm.recreates[0].Ports; len(got) != 1 || got[0] != "127.0.0.1:80:80" {
		t.Errorf("recreate spec ports: got %+v", got)
	}
}

func TestDeploymentHandler_RecreatesOnImageIDDrift(t *testing.T) {
	// Build-mode scenario: manifest text is identical across applies
	// (image = "vd-web:latest"), but each rebuild refreshes the tag so
	// the image ID underneath flips. Spec-hash can't see this — only an
	// ID comparison can — and without catching it, the container keeps
	// serving yesterday's code.
	store := newMemStore()

	spec := deploymentSpec{Image: "vd-web:latest", Networks: []string{"voodu0"}}
	pre, _ := json.Marshal(DeploymentStatus{Image: spec.Image, SpecHash: deploymentSpecHash(spec)})
	_ = store.PutStatus(context.Background(), KindDeployment, "vd-web", pre)

	cm := &fakeContainers{
		exists: map[string]bool{"vd-web-0": true},
		// Slot 0 is still running the layer sha it was created with, but
		// the tag "vd-web:latest" now points at a freshly-built layer.
		// The reconciler checks slot 0 as the canary for image-id drift.
		containerImageIDs: map[string]string{"vd-web-0": "sha256:oldlayer"},
		tagImageIDs:       map[string]string{"vd-web:latest": "sha256:newlayer"},
	}

	h := &DeploymentHandler{
		Store:       store,
		Log:         quietLogger(),
		WriteEnv:    func(string, []string) (bool, error) { return false, nil },
		EnvFilePath: func(app string) string { return "/tmp/" + app + ".env" },
		Containers:  cm,
	}

	ev := putEvent(t, KindDeployment, "vd-web", deploymentSpec{Image: "vd-web:latest"})

	if err := h.Handle(context.Background(), ev); err != nil {
		t.Fatalf("handle: %v", err)
	}

	if len(cm.recreates) != 1 {
		t.Fatalf("expected 1 recreate on image-id drift, got %d", len(cm.recreates))
	}

	if cm.recreates[0].Image != "vd-web:latest" {
		t.Errorf("recreate image: got %q", cm.recreates[0].Image)
	}
}

func TestDeploymentHandler_NoRecreateWhenImageIDsMatch(t *testing.T) {
	// Steady state: same tag, same underlying ID — replay must be a
	// no-op even though the tag happens to be a mutable `<app>:latest`.
	// Without this test, a naive implementation that always recreates on
	// build-mode tags would churn the container on every reconcile.
	store := newMemStore()

	spec := deploymentSpec{Image: "vd-web:latest", Networks: []string{"voodu0"}}
	pre, _ := json.Marshal(DeploymentStatus{Image: spec.Image, SpecHash: deploymentSpecHash(spec)})
	_ = store.PutStatus(context.Background(), KindDeployment, "vd-web", pre)

	cm := &fakeContainers{
		exists:            map[string]bool{"vd-web-0": true},
		containerImageIDs: map[string]string{"vd-web-0": "sha256:same"},
		tagImageIDs:       map[string]string{"vd-web:latest": "sha256:same"},
	}

	h := &DeploymentHandler{
		Store:       store,
		Log:         quietLogger(),
		WriteEnv:    func(string, []string) (bool, error) { return false, nil },
		EnvFilePath: func(app string) string { return "/tmp/" + app + ".env" },
		Containers:  cm,
	}

	ev := putEvent(t, KindDeployment, "vd-web", deploymentSpec{Image: "vd-web:latest"})

	h.Handle(context.Background(), ev)

	if len(cm.recreates) != 0 {
		t.Errorf("matching image IDs must not recreate, got %+v", cm.recreates)
	}
}

func TestDeploymentHandler_NoRecreateWhenImagesMatch(t *testing.T) {
	store := newMemStore()

	// Pre-seed status that matches the manifest we're about to apply.
	// This mirrors the steady-state: status was persisted on first
	// create, now the controller is replaying the same manifest.
	// Networks is explicit (matches the handler's default normalization
	// of empty → [voodu0]) so the hash we pre-seed matches the hash the
	// handler will recompute after apply() runs its normalization.
	spec := deploymentSpec{Image: "img:1", Networks: []string{"voodu0"}}
	pre, _ := json.Marshal(DeploymentStatus{Image: spec.Image, SpecHash: deploymentSpecHash(spec)})
	_ = store.PutStatus(context.Background(), KindDeployment, "api", pre)

	cm := &fakeContainers{
		exists: map[string]bool{"api-0": true},
		images: map[string]string{"api-0": "img:1"},
	}

	h := &DeploymentHandler{
		Store:       store,
		Log:         quietLogger(),
		WriteEnv:    func(string, []string) (bool, error) { return true, nil },
		EnvFilePath: func(app string) string { return "/tmp/" + app + ".env" },
		Containers:  cm,
	}

	ev := putEvent(t, KindDeployment, "api", deploymentSpec{
		Image: "img:1",
		Env:   map[string]string{"X": "new"},
	})

	h.Handle(context.Background(), ev)

	if len(cm.recreates) != 0 {
		t.Errorf("no recreate expected when image matches, got %+v", cm.recreates)
	}

	// Env changed, container existed → plain restart picks up the env.
	if len(cm.restarts) != 1 {
		t.Errorf("expected 1 restart for env change, got %+v", cm.restarts)
	}
}

func TestDeploymentHandler_FirstReconcileBaselinesWithoutRecreate(t *testing.T) {
	store := newMemStore()

	cm := &fakeContainers{
		// Slot already exists (imagine: controller upgrade onto a server
		// where deployments ran under the pre-hash code path).
		exists: map[string]bool{"api-0": true},
		images: map[string]string{"api-0": "img:old"},
	}

	h := &DeploymentHandler{
		Store:       store,
		Log:         quietLogger(),
		WriteEnv:    func(string, []string) (bool, error) { return false, nil },
		EnvFilePath: func(app string) string { return "/tmp/" + app + ".env" },
		Containers:  cm,
	}

	ev := putEvent(t, KindDeployment, "api", deploymentSpec{Image: "img:new"})

	h.Handle(context.Background(), ev)

	// First-time reconcile with no status must not recreate — the
	// running container might predate status persistence entirely, and
	// churning every pre-existing deploy on upgrade is a surprise.
	if len(cm.recreates) != 0 {
		t.Errorf("first reconcile without status must not recreate, got %+v", cm.recreates)
	}

	// But it MUST persist a baseline hash so the next real drift gets
	// caught. Without this write, every reconcile would re-baseline.
	raw, _ := store.GetStatus(context.Background(), KindDeployment, "api")
	if raw == nil {
		t.Fatal("expected status baseline to be persisted on first reconcile")
	}

	var st DeploymentStatus
	_ = json.Unmarshal(raw, &st)

	if st.SpecHash == "" {
		t.Errorf("persisted status missing hash: %+v", st)
	}
}

func TestDatabaseHandler_LogsVersionDriftOnReplay(t *testing.T) {
	store := newMemStore()

	// Pre-seed status from a prior `create` that persisted version=16.
	pre, _ := json.Marshal(DatabaseStatus{
		Engine:  "postgres",
		Version: "16",
		Data:    map[string]any{"url": "x"},
	})
	_ = store.PutStatus(context.Background(), KindDatabase, "main", pre)

	inv := &fakeInvoker{}

	var logs strings.Builder

	h := &DatabaseHandler{Store: store, Invoker: inv, Log: log.New(&logs, "", 0)}

	// New desired spec wants version 17 — that's drift.
	ev := putEvent(t, KindDatabase, "main", databaseSpec{Engine: "postgres", Version: "17"})

	_ = h.Handle(context.Background(), ev)

	// Handler must stay idempotent: no plugin re-invoke.
	if len(inv.calls) != 0 {
		t.Errorf("drift detection must not re-invoke plugin, got %d calls", len(inv.calls))
	}

	if !strings.Contains(logs.String(), "version drift") {
		t.Errorf("expected version-drift warning in logs:\n%s", logs.String())
	}
}

func TestDeploymentHandler_DoesNotRestartFreshContainer(t *testing.T) {
	store := newMemStore()

	cm := &fakeContainers{} // empty — Ensure will create

	h := &DeploymentHandler{
		Store:       store,
		Log:         quietLogger(),
		WriteEnv:    func(string, []string) (bool, error) { return true, nil }, // changed
		EnvFilePath: func(app string) string { return "/tmp/" + app + ".env" },
		Containers:  cm,
	}

	ev := putEvent(t, KindDeployment, "api", deploymentSpec{
		Image: "img:1",
		Env:   map[string]string{"X": "y"},
	})

	h.Handle(context.Background(), ev)

	// Fresh container comes up with the new env, so restarting right
	// after Ensure would be redundant.
	if len(cm.restarts) != 0 {
		t.Errorf("just-created container must not be restarted, got %+v", cm.restarts)
	}
}

func TestDeploymentHandler_EmptyImageDefaultsToAppLatest(t *testing.T) {
	store := newMemStore()

	cm := &fakeContainers{}

	h := &DeploymentHandler{
		Store:       store,
		Log:         quietLogger(),
		WriteEnv:    func(string, []string) (bool, error) { return false, nil },
		EnvFilePath: func(app string) string { return "/tmp/" + app + ".env" },
		Containers:  cm,
	}

	// Build-mode (no image, source pushed via git) produces <app>:latest.
	// The controller never sees the build, so it resolves the image by
	// convention. Without this default, the build-mode reconcile is a
	// no-op and the container never starts.
	ev := putEvent(t, KindDeployment, "vd-web", deploymentSpec{
		Env: map[string]string{"X": "1"},
	})

	h.Handle(context.Background(), ev)

	if len(cm.ensures) != 1 {
		t.Fatalf("ensure should fire once with defaulted image, got %d calls", len(cm.ensures))
	}

	if cm.ensures[0].Image != "vd-web:latest" {
		t.Errorf("expected image vd-web:latest, got %q", cm.ensures[0].Image)
	}
}

func TestDeploymentHandler_ReplicasSpawnsEveryIndexedSlot(t *testing.T) {
	// A deployment with replicas=3 must yield exactly three Ensure calls,
	// one per `<app>-<N>` slot, in index order. Without this the ingress
	// would only have slot 0 to dial, defeating the point of replicas.
	store := newMemStore()

	cm := &fakeContainers{}

	h := &DeploymentHandler{
		Store:       store,
		Log:         quietLogger(),
		WriteEnv:    func(string, []string) (bool, error) { return false, nil },
		EnvFilePath: func(app string) string { return "/tmp/" + app + ".env" },
		Containers:  cm,
	}

	ev := putEvent(t, KindDeployment, "api", deploymentSpec{
		Image:    "img:1",
		Replicas: 3,
	})

	h.Handle(context.Background(), ev)

	if len(cm.ensures) != 3 {
		t.Fatalf("expected 3 ensures for replicas=3, got %d", len(cm.ensures))
	}

	for i, e := range cm.ensures {
		want := fmt.Sprintf("api-%d", i)

		if e.Name != want {
			t.Errorf("ensure[%d] name: got %q want %q", i, e.Name, want)
		}
	}
}

func TestDeploymentHandler_ScaleDownRemovesExtraSlots(t *testing.T) {
	// replicas went 3 → 1. The two extra slots must be Removed; the
	// surviving slot stays untouched. Runs before any spec-drift work
	// so the rollout loop doesn't churn slots that are already gone.
	store := newMemStore()

	cm := &fakeContainers{
		exists: map[string]bool{
			"api-0": true,
			"api-1": true,
			"api-2": true,
		},
		images: map[string]string{
			"api-0": "img:1",
			"api-1": "img:1",
			"api-2": "img:1",
		},
	}

	// Baseline status so no spec-drift recreate triggers.
	spec := deploymentSpec{Image: "img:1", Networks: []string{"voodu0"}}
	pre, _ := json.Marshal(DeploymentStatus{Image: spec.Image, SpecHash: deploymentSpecHash(spec)})
	_ = store.PutStatus(context.Background(), KindDeployment, "api", pre)

	h := &DeploymentHandler{
		Store:       store,
		Log:         quietLogger(),
		WriteEnv:    func(string, []string) (bool, error) { return false, nil },
		EnvFilePath: func(app string) string { return "/tmp/" + app + ".env" },
		Containers:  cm,
	}

	ev := putEvent(t, KindDeployment, "api", deploymentSpec{Image: "img:1", Replicas: 1})

	h.Handle(context.Background(), ev)

	sort.Strings(cm.removes)

	if want := []string{"api-1", "api-2"}; len(cm.removes) != len(want) ||
		cm.removes[0] != want[0] || cm.removes[1] != want[1] {
		t.Errorf("scale-down removes: got %+v want %+v", cm.removes, want)
	}

	if len(cm.recreates) != 0 {
		t.Errorf("scale-down must not recreate surviving slots, got %+v", cm.recreates)
	}
}

func TestDeploymentHandler_PrunesLegacyBareNameContainer(t *testing.T) {
	// A pre-slot deployment left behind a bare-name `<app>` container.
	// The new reconciler must remove it (port/volume collisions with
	// `<app>-0` are inevitable) and spawn the indexed slot cleanly.
	store := newMemStore()

	cm := &fakeContainers{
		exists: map[string]bool{"api": true},
	}

	h := &DeploymentHandler{
		Store:       store,
		Log:         quietLogger(),
		WriteEnv:    func(string, []string) (bool, error) { return false, nil },
		EnvFilePath: func(app string) string { return "/tmp/" + app + ".env" },
		Containers:  cm,
	}

	ev := putEvent(t, KindDeployment, "api", deploymentSpec{Image: "img:1"})

	h.Handle(context.Background(), ev)

	if len(cm.removes) != 1 || cm.removes[0] != "api" {
		t.Errorf("expected legacy removal of %q, got %+v", "api", cm.removes)
	}

	if len(cm.ensures) != 1 || cm.ensures[0].Name != "api-0" {
		t.Errorf("expected ensure of api-0 after legacy prune, got %+v", cm.ensures)
	}
}

func TestIngressApplyEnv_EmitsMultiUpstreamAndLB(t *testing.T) {
	env := ingressApplyEnv(
		"api",
		ingressSpec{
			Host:    "api.example.com",
			Service: "api",
			Port:    3000,
			LB:      &ingressLB{Policy: "least_conn", Interval: "5s"},
		},
		upstreamResolution{
			Upstreams:       []string{"api-0:3000", "api-1:3000"},
			HealthCheckPath: "/healthz",
		},
	)

	if got := env[plugin.EnvIngressUpstreams]; got != `["api-0:3000","api-1:3000"]` {
		t.Errorf("UPSTREAMS: got %q", got)
	}

	if got := env[plugin.EnvIngressLBPolicy]; got != "least_conn" {
		t.Errorf("LB_POLICY: got %q", got)
	}

	if got := env[plugin.EnvIngressLBInterval]; got != "5s" {
		t.Errorf("LB_INTERVAL: got %q", got)
	}

	if got := env[plugin.EnvIngressHealthCheckPath]; got != "/healthz" {
		t.Errorf("HC_PATH: got %q", got)
	}
}

func putEvent(t *testing.T, kind Kind, name string, spec any) WatchEvent {
	t.Helper()

	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}

	scope := ""
	if IsScoped(kind) {
		scope = "test"
	}

	return WatchEvent{
		Type:  WatchPut,
		Kind:  kind,
		Scope: scope,
		Name:  name,
		Manifest: &Manifest{
			Kind:  kind,
			Scope: scope,
			Name:  name,
			Spec:  json.RawMessage(raw),
		},
	}
}

func quietLogger() *log.Logger {
	return log.New(io.Discard, "", 0)
}

// Compile-time checks — the reconciler's seams are small and catching
// signature drift here is cheaper than at runtime.
var (
	_ PluginInvoker    = (*DirInvoker)(nil)
	_ PluginInvoker    = (*fakeInvoker)(nil)
	_ ContainerManager = DockerContainerManager{}
	_ ContainerManager = (*fakeContainers)(nil)
)

// Helper used nowhere in production — keep it near the fake it uses.
func (f *fakeInvoker) describe() string {
	var b strings.Builder
	for _, c := range f.calls {
		fmt.Fprintf(&b, "%s.%s(%v) ", c.Plugin, c.Command, c.Args)
	}
	return b.String()
}
