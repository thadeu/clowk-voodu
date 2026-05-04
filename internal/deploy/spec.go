package deploy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"go.voodu.clowk.in/internal/controller"
	"go.voodu.clowk.in/internal/lang"
)

// Spec is the server-side pipeline contract: everything receive-pack
// and the deploy pipeline need to turn a tarball into a running image.
// Its shape mirrors manifest.DeploymentSpec (flat root for pipeline +
// container concerns; one optional lang block carrying build-time
// inputs) but is re-declared here to keep internal/deploy free of a
// cyclic import through internal/manifest → internal/controller.
//
// Conversion from the wire spec (json.RawMessage in etcd) happens in
// FetchSpec below — one place, one shape.
type Spec struct {
	Image      string
	Dockerfile string
	Path       string
	Workdir    string

	Env         map[string]string
	Ports       []string
	Volumes     []string
	NetworkMode string

	PostDeploy   []string
	KeepReleases int

	Lang *LangSpec
}

type LangSpec struct {
	Name       string
	Version    string
	Entrypoint string
	BuildArgs  map[string]string
}

// toBuildSpec strips pipeline-only fields and yields what lang handlers
// consume. The lang block is forwarded as-is.
func (s *Spec) toBuildSpec() *lang.BuildSpec {
	if s == nil {
		return &lang.BuildSpec{}
	}

	out := &lang.BuildSpec{
		Image:       s.Image,
		Dockerfile:  s.Dockerfile,
		Path:        s.Path,
		Workdir:     s.Workdir,
		Env:         s.Env,
		Ports:       s.Ports,
		Volumes:     s.Volumes,
		NetworkMode: s.NetworkMode,
	}

	if s.Lang != nil {
		out.Lang = &lang.LangBuildSpec{
			Name:       s.Lang.Name,
			Version:    s.Lang.Version,
			Entrypoint: s.Lang.Entrypoint,
			BuildArgs:  s.Lang.BuildArgs,
		}
	}

	return out
}

// wireDeploymentSpec is the JSON shape the controller persists under
// /desired/deployments/<scope>/<name>/spec. We re-declare it here
// (instead of importing internal/manifest) because manifest imports
// controller, and controller imports deploy indirectly via the API
// handlers — importing manifest here would close a dep cycle.
//
// Keep this in lockstep with manifest.DeploymentSpec; field names and
// JSON tags must match exactly.
type wireDeploymentSpec struct {
	Image        string            `json:"image,omitempty"`
	Dockerfile   string            `json:"dockerfile,omitempty"`
	Path         string            `json:"path,omitempty"`
	Workdir      string            `json:"workdir,omitempty"`
	Env          map[string]string `json:"env,omitempty"`
	Ports        []string          `json:"ports,omitempty"`
	Volumes      []string          `json:"volumes,omitempty"`
	NetworkMode  string            `json:"network_mode,omitempty"`
	PostDeploy   []string          `json:"post_deploy,omitempty"`
	KeepReleases int               `json:"keep_releases,omitempty"`

	Lang *wireLangSpec `json:"lang,omitempty"`
}

// wireStatefulsetSpec is the build-relevant subset of
// manifest.StatefulsetSpec. Statefulset workloads can also be
// build-mode (postgres + pgvector compiled inline, redis with custom
// modules, etc.) — same Image/Dockerfile/Workdir/Path/Lang surface as
// deployment, just no PostDeploy/KeepReleases (statefulsets don't run
// post-deploy hooks; they use init_containers / pre_stop_hook for
// equivalent concerns).
//
// Keep in lockstep with manifest.StatefulsetSpec; field names and JSON
// tags must match exactly. Same dep-cycle reasoning as
// wireDeploymentSpec applies — this lives here, not in manifest.
type wireStatefulsetSpec struct {
	Image       string            `json:"image,omitempty"`
	Dockerfile  string            `json:"dockerfile,omitempty"`
	Path        string            `json:"path,omitempty"`
	Workdir     string            `json:"workdir,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	Ports       []string          `json:"ports,omitempty"`
	Volumes     []string          `json:"volumes,omitempty"`
	NetworkMode string            `json:"network_mode,omitempty"`

	Lang *wireLangSpec `json:"lang,omitempty"`
}

type wireLangSpec struct {
	Name       string            `json:"name,omitempty"`
	Version    string            `json:"version,omitempty"`
	Entrypoint string            `json:"entrypoint,omitempty"`
	BuildArgs  map[string]string `json:"build_args,omitempty"`
}

// ErrSpecAmbiguous is returned by FetchSpec when both a deployment AND
// a statefulset exist with the same (scope, name). receive-pack receives
// only `<scope>/<name>` over SSH (no kind), so the resolver picks the
// unique match — ambiguity means the operator has to disambiguate by
// changing one of the two. This is rare in practice (mixing the two
// kinds on the same name is unusual) but the error path keeps it loud
// instead of silently picking one.
var ErrSpecAmbiguous = fmt.Errorf("ambiguous build target: both deployment and statefulset exist with this scope/name")

// FetchSpec queries the local controller HTTP API for the build manifest
// of <scope>/<name> and returns the unmarshalled Spec. Searches across
// the two build-capable kinds (deployment + statefulset) and returns
// the unique match.
//
// Returns (nil, nil) when the controller does not know about either —
// a valid state for brand-new apps whose first receive-pack arrives
// before `voodu apply`. Callers treat it as "use defaults and
// auto-detect".
//
// Returns ErrSpecAmbiguous if both a deployment AND a statefulset
// exist with the same (scope, name).
//
// Other failures (controller unreachable, malformed response) are
// surfaced as errors: the operator needs to see that the build-config
// source of truth is broken, not silently fall back.
func FetchSpec(controllerURL, scope, name string) (*Spec, error) {
	if controllerURL == "" {
		return nil, nil
	}

	dSpec, err := fetchSpecForKind(controllerURL, controller.KindDeployment, scope, name)
	if err != nil {
		return nil, err
	}

	sSpec, err := fetchSpecForKind(controllerURL, controller.KindStatefulset, scope, name)
	if err != nil {
		return nil, err
	}

	if dSpec != nil && sSpec != nil {
		return nil, ErrSpecAmbiguous
	}

	if dSpec != nil {
		return dSpec, nil
	}

	return sSpec, nil
}

// fetchSpecForKind queries the controller for one specific kind. Returns
// (nil, nil) when no match — caller decides what to do across kinds.
func fetchSpecForKind(controllerURL string, kind controller.Kind, scope, name string) (*Spec, error) {
	q := url.Values{}
	q.Set("kind", string(kind))
	q.Set("name", name)

	if scope != "" {
		q.Set("scope", scope)
	}

	full := controllerURL + "/apply?" + q.Encode()

	req, err := http.NewRequest(http.MethodGet, full, nil)
	if err != nil {
		return nil, err
	}

	client := &http.Client{Timeout: 5 * time.Second}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("controller GET /apply: %w", err)
	}

	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("controller GET /apply returned %d", resp.StatusCode)
	}

	var env struct {
		Data []controller.Manifest `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return nil, fmt.Errorf("decode /apply response: %w", err)
	}

	for _, m := range env.Data {
		if m.Kind != kind || m.Name != name {
			continue
		}

		if scope != "" && m.Scope != scope {
			continue
		}

		switch kind {
		case controller.KindDeployment:
			var wire wireDeploymentSpec

			if err := json.Unmarshal(m.Spec, &wire); err != nil {
				return nil, fmt.Errorf("decode deployment spec: %w", err)
			}

			return specFromWire(wire), nil

		case controller.KindStatefulset:
			var wire wireStatefulsetSpec

			if err := json.Unmarshal(m.Spec, &wire); err != nil {
				return nil, fmt.Errorf("decode statefulset spec: %w", err)
			}

			return specFromStatefulsetWire(wire), nil
		}
	}

	return nil, nil
}

func specFromWire(w wireDeploymentSpec) *Spec {
	s := &Spec{
		Image:        w.Image,
		Dockerfile:   w.Dockerfile,
		Path:         w.Path,
		Workdir:      w.Workdir,
		Env:          w.Env,
		Ports:        w.Ports,
		Volumes:      w.Volumes,
		NetworkMode:  w.NetworkMode,
		PostDeploy:   w.PostDeploy,
		KeepReleases: w.KeepReleases,
	}

	if w.Lang != nil {
		s.Lang = &LangSpec{
			Name:       w.Lang.Name,
			Version:    w.Lang.Version,
			Entrypoint: w.Lang.Entrypoint,
			BuildArgs:  w.Lang.BuildArgs,
		}
	}

	return s
}

// specFromStatefulsetWire mirrors specFromWire — statefulsets share
// every build-relevant field with deployments except PostDeploy and
// KeepReleases (intentionally omitted; statefulsets don't have those
// hooks).
func specFromStatefulsetWire(w wireStatefulsetSpec) *Spec {
	s := &Spec{
		Image:       w.Image,
		Dockerfile:  w.Dockerfile,
		Path:        w.Path,
		Workdir:     w.Workdir,
		Env:         w.Env,
		Ports:       w.Ports,
		Volumes:     w.Volumes,
		NetworkMode: w.NetworkMode,
	}

	if w.Lang != nil {
		s.Lang = &LangSpec{
			Name:       w.Lang.Name,
			Version:    w.Lang.Version,
			Entrypoint: w.Lang.Entrypoint,
			BuildArgs:  w.Lang.BuildArgs,
		}
	}

	return s
}
