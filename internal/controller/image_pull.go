package controller

import (
	"context"
	"encoding/json"
	"fmt"
)

// ImagePuller is the seam `POST /apply?force=true` dispatches through
// to refresh registry-mode images before the manifests land in the
// store. DockerContainerManager implements it in production (Pull →
// `docker pull`, ImageID → `docker image inspect`); tests substitute a
// fake so the apply path never touches a daemon.
//
// The pull deliberately runs inside the controller process rather than
// in the CLI: the controller's HOME owns the docker config.json that
// RegistryHandler rewrites on every `registry "name" { … }` reconcile,
// so a pull from here authenticates against ECR / GHCR / Harbor with
// the same credentials the container-create path already uses.
//
// Nil on the API disables force-pull entirely — the apply still runs,
// it just reports no `image_pulls`.
type ImagePuller interface {
	// Pull fetches ref from its registry. ctx bounds the download so a
	// disconnected apply doesn't leave a multi-GB transfer running.
	Pull(ctx context.Context, ref string) error

	// ImageID resolves ref to its local sha256 id. Empty string (with
	// a nil error) means the image isn't present locally.
	ImageID(ref string) (string, error)
}

// ImagePullResult is one entry of the `image_pulls` array `/apply?force=true`
// returns. The CLI renders it as `pulled <image>` / `<image> already
// up to date`, so the operator sees which tag actually moved.
type ImagePullResult struct {
	Image string `json:"image"`

	// Updated reports whether the tag resolves to a different image ID
	// than it did before the pull. False means the local copy was
	// already the newest digest — no replica recreate will follow.
	Updated bool `json:"updated"`

	// Warning carries a non-fatal pull failure: the registry was
	// unreachable (or refused us) BUT the image is present locally, so
	// the apply proceeds with the copy already on the host. Fatal
	// failures — no local copy to fall back on — abort the apply
	// instead of landing here.
	Warning string `json:"warning,omitempty"`
}

// imageBearingKinds is the set of core kinds whose spec carries a
// top-level `image` field worth re-pulling. Build-mode deployments
// leave it empty (their image comes from receive-pack's docker build),
// so they fall out of the set naturally without a mode check here.
var imageBearingKinds = map[Kind]bool{
	KindDeployment:  true,
	KindStatefulset: true,
	KindJob:         true,
	KindCronJob:     true,
}

// collectPullableImages walks the post-expansion manifest batch and
// returns every distinct registry image it names, in declaration
// order. Order is stable so the CLI's output reads the same way twice
// for the same file.
func collectPullableImages(manifests []*Manifest) []string {
	var (
		images []string
		seen   = map[string]bool{}
	)

	for _, m := range manifests {
		if m == nil || !imageBearingKinds[m.Kind] || len(m.Spec) == 0 {
			continue
		}

		var probe struct {
			Image string `json:"image"`
		}

		if err := json.Unmarshal(m.Spec, &probe); err != nil {
			continue
		}

		if probe.Image == "" || seen[probe.Image] {
			continue
		}

		seen[probe.Image] = true
		images = append(images, probe.Image)
	}

	return images
}

// forcePullImages re-pulls every image the batch names and reports
// which tags moved. Called only for `--force` applies — the steady
// state trusts the tag already on the host, which is what makes a
// mutable tag (`:latest`, `:main`) go stale after a registry push.
//
// A pull that fails is fatal ONLY when the host has no local copy to
// fall back on: force asked for the newest bytes and we can neither
// fetch them nor run what's there. When a local copy exists the
// failure degrades to a warning and the apply continues with it —
// the same posture `docker run` takes.
//
// Returning before any Store.Put is deliberate: a fatal pull leaves
// desired state untouched, so a retry after fixing the registry
// credentials is a clean re-apply rather than a half-applied batch.
func (a *API) forcePullImages(ctx context.Context, manifests []*Manifest) ([]ImagePullResult, error) {
	if a.Images == nil {
		return nil, nil
	}

	images := collectPullableImages(manifests)
	if len(images) == 0 {
		return nil, nil
	}

	results := make([]ImagePullResult, 0, len(images))

	for _, img := range images {
		before, _ := a.Images.ImageID(img)

		res := ImagePullResult{Image: img}

		if err := a.Images.Pull(ctx, img); err != nil {
			if before == "" {
				return nil, fmt.Errorf("force pull %s: %w", img, err)
			}

			res.Warning = err.Error()
			results = append(results, res)

			continue
		}

		after, _ := a.Images.ImageID(img)
		res.Updated = before != after

		results = append(results, res)
	}

	return results, nil
}
