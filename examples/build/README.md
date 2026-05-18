# build mode — building images from source

Voodu has two source modes for a workload: pull a pre-built image from a registry (`image = "..."`), or build the image from your repo (`build { ... }`). They're mutually exclusive — declaring both is a parse error.

This directory shows the `build { ... }` shape end-to-end.

## When to use build mode

- You don't have (or don't want) a CI pipeline publishing images.
- The Dockerfile lives next to the code, and `vd apply` from the repo root is your release flow.
- You're prototyping; baking an image per commit on the operator's machine is fast enough.

When you outgrow it (multi-host, parallel deploys, signed images), switch to `image = "ghcr.io/..."` and let your CI publish.

## The block

```hcl
deployment "scope" "name" {
  build {
    context    = "apps/api"        # directory sent to `docker build`
    dockerfile = "Dockerfile.api"  # relative to context, defaults to "Dockerfile"
    path       = "cmd/api"         # voodu-only, used by auto-generated Dockerfiles
    args       = { SERVICE = "api" }   # docker --build-arg

    lang {
      name    = "go"               # picks language handler
      version = "1.25"
    }
  }
}
```

Field semantics:

- **`context`** — directory whose contents go to `docker build` as the context. Defaults to `"."` when `build {}` is declared without it. Matches docker-compose's `build.context`.
- **`dockerfile`** — non-default Dockerfile name, relative to `context`. Empty → look for `./Dockerfile`; if missing, language handlers may auto-generate one.
- **`path`** — voodu-specific. Used ONLY by auto-generated Dockerfiles (the Go handler emits `go build ./<path>`). Custom Dockerfiles ignore this — they handle their own `COPY` / `WORKDIR`.
- **`args`** — docker `--build-arg KEY=value` map. Matches docker-compose's `build.args`. Works for any Dockerfile.
- **`lang { }`** — optional runtime hint. Picks which build handler to dispatch. Nested inside `build` because it's a build-time concern. Omit it and voodu auto-detects from marker files (`go.mod`, `Gemfile`, `package.json`, ...) in `context`.

## Auto-detect (the terse shape)

The minimum spell:

```hcl
deployment "scope" "name" {}
```

No `image`, no `build {}` — `applyDefaults` synthesises `build { context = "." }` and lets language handlers sniff the runtime. Practically equivalent to:

```hcl
deployment "scope" "name" {
  build {
    context = "."
  }
}
```

Use this when the repo IS the app, the runtime is auto-detectable, and you don't need custom build args.

## Examples in this directory

| file | what it shows |
|---|---|
| [`go-monorepo.hcl`](./go-monorepo.hcl) | Three Go services in one repo, each with its own `build.context` and `args`. The common monorepo case. |
| [`custom-dockerfile.hcl`](./custom-dockerfile.hcl) | FreeSWITCH-style workload: custom Dockerfile + `args` + `cap_add` + `extra_hosts`. |
| [`auto-detect.hcl`](./auto-detect.hcl) | Minimal "ship this repo, figure the rest out" shape. |
| [`statefulset-pgvector.hcl`](./statefulset-pgvector.hcl) | Postgres + pgvector built inline (no separate CI to publish the image). |

## Running an example

```bash
cd examples/build
vd apply -f go-monorepo.hcl
```

The CLI streams the source as a tarball over SSH to `voodu receive-pack` on the controller, which runs the build pipeline and tags `<scope>-<name>:latest` for the workload to pull. `vd apply -v` shows the docker buildx output if you want to see what's happening.

## Build args vs lang.version

`lang.version` flows into auto-generated Dockerfiles where the handler knows what to do with it (Go base image tag, Ruby version pin, ...). `build.args` are raw `--build-arg` pairs that work for any Dockerfile.

The Golang handler auto-injects `GOOS=linux`, `GOARCH=<host>`, `CGO_ENABLED=0` as defaults; entries in `build.args` override them:

```hcl
build {
  args = {
    GOOS        = "darwin"   # override default
    CGO_ENABLED = "1"        # cgo build
  }

  lang {
    name = "go"
  }
}
```

## Mutual exclusivity

This errors at parse time:

```hcl
deployment "scope" "api" {
  image = "ghcr.io/me/api:v1"   # registry mode

  build {                        # AND build mode — pick one
    context = "."
  }
}
```

```
Error: image and build {} are mutually exclusive: use image to pull
from a registry, or build {} to build from source — not both
```
