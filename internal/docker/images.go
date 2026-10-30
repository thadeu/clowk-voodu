// images.go is the read and delete surface the image pruner stands on.
//
// Unlike GetImageID, nothing here is fail-soft. GetImageID answers "" for a
// daemon hiccup, which is fine when the caller retries on the next tick and
// wrong for a caller that decides what to DELETE: a protected image would read
// as "not here" and lose its protection. So these return the error, and the
// pruner stops.

package docker

import (
	"context"
	"errors"
	"fmt"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	dockerclient "github.com/moby/moby/client"
)

// ErrImageInUse is what RemoveImage answers when the daemon refuses because a
// container still references the image. A normal outcome of a race with a
// deploy, not a failure.
var ErrImageInUse = errors.New("image in use")

// Image is one local image: its tags plus what the pruner needs to judge age.
type Image struct {
	ID      string
	Tags    []string
	Created time.Time
}

// ListImages returns every tagged or dangling image. Intermediate layers stay
// out: they are not images an operator made, and they go with their child.
func ListImages(ctx context.Context) ([]Image, error) {
	cli, err := getDockerClient()
	if err != nil {
		return nil, err
	}

	result, err := cli.ImageList(ctx, dockerclient.ImageListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list images: %w", err)
	}

	out := make([]Image, 0, len(result.Items))

	for _, item := range result.Items {
		img := Image{ID: item.ID, Created: time.Unix(item.Created, 0)}

		for _, tag := range item.RepoTags {
			if tag != "<none>:<none>" {
				img.Tags = append(img.Tags, tag)
			}
		}

		out = append(out, img)
	}

	return out, nil
}

// ContainerImageIDs returns the image of every container on the host, STOPPED
// ones included: a stopped pod comes back through `vd start`, and the daemon
// refuses to delete an image a stopped container references anyway.
func ContainerImageIDs(ctx context.Context) ([]string, error) {
	cli, err := getDockerClient()
	if err != nil {
		return nil, err
	}

	result, err := cli.ContainerList(ctx, dockerclient.ContainerListOptions{All: true})
	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}

	out := make([]string, 0, len(result.Items))

	for _, item := range result.Items {
		out = append(out, item.ImageID)
	}

	return out, nil
}

// ResolveImageID turns a reference into the local image ID. Empty string and
// nil error mean the reference names nothing here. A malformed reference also
// names nothing: a manifest field that is not a valid image is not a reason to
// stop every prune on this host.
func ResolveImageID(ctx context.Context, ref string) (string, error) {
	cli, err := getDockerClient()
	if err != nil {
		return "", err
	}

	resp, err := cli.ImageInspect(ctx, ref)
	if err != nil {
		if cerrdefs.IsNotFound(err) || cerrdefs.IsInvalidArgument(err) {
			return "", nil
		}

		return "", fmt.Errorf("inspect image %s: %w", ref, err)
	}

	return resp.ID, nil
}

// ImageLastTagged is when the image last received a tag: the moment a build or
// a pull finished. `Created` alone cannot answer that, because a cached build
// or an old registry image lands today with a creation date from months ago.
//
// Zero when the daemon doesn't record it.
func ImageLastTagged(ctx context.Context, id string) (time.Time, error) {
	cli, err := getDockerClient()
	if err != nil {
		return time.Time{}, err
	}

	resp, err := cli.ImageInspect(ctx, id)
	if err != nil {
		return time.Time{}, fmt.Errorf("inspect image %s: %w", id, err)
	}

	if resp.Metadata.LastTagTime.IsZero() {
		return time.Time{}, nil
	}

	return resp.Metadata.LastTagTime, nil
}

// RemoveImage drops one tag, or the image itself when ref is an ID. Never
// forced: the daemon's refusal is the last guard against deleting what a
// container uses, and it is the one guard that cannot go stale.
func RemoveImage(ctx context.Context, ref string) error {
	cli, err := getDockerClient()
	if err != nil {
		return err
	}

	_, err = cli.ImageRemove(ctx, ref, dockerclient.ImageRemoveOptions{PruneChildren: true})

	switch {
	case err == nil, cerrdefs.IsNotFound(err):
		return nil
	case cerrdefs.IsConflict(err):
		return fmt.Errorf("%w: %s", ErrImageInUse, ref)
	}

	return fmt.Errorf("remove image %s: %w", ref, err)
}

// ImagesDiskSize is the disk the image store uses, shared layers counted once.
// The same number as the Images row of `docker system df`.
func ImagesDiskSize(ctx context.Context) (int64, error) {
	cli, err := getDockerClient()
	if err != nil {
		return 0, err
	}

	result, err := cli.DiskUsage(ctx, dockerclient.DiskUsageOptions{Images: true})
	if err != nil {
		return 0, fmt.Errorf("disk usage: %w", err)
	}

	return result.Images.TotalSize, nil
}
