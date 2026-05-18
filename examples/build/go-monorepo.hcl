// Monorepo with three Go services, each built from its own subdir.
//
// Repo layout (illustrative):
//
//   apps/
//     api/        cmd/api/main.go     → builds with `go build ./cmd/api`
//     worker/     cmd/worker/main.go  → builds with `go build ./cmd/worker`
//     scheduler/  cmd/scheduler/main.go
//   internal/...
//   go.mod
//
// Each deployment picks a different `build.context` so the tarball
// only includes the relevant subtree — faster uploads + tighter
// docker build cache. `build.path` tells the Go handler which
// subdirectory inside the context to compile (auto-generated
// Dockerfile uses `go build ./<path>`).
//
// `args` overrides the Golang handler's defaults — useful when you
// need a non-linux/amd64 build, cgo-enabled binaries, or to bake a
// git SHA into the binary via `-ldflags`.

deployment "shop" "api" {
  replicas = 2
  ports    = ["8080"]

  env = {
    PORT = "8080"
  }

  build {
    context = "apps/api"
    path    = "cmd/api"

    args = {
      GIT_SHA = "${GIT_SHA:-dev}"   // shell interpolation: set GIT_SHA=$(git rev-parse HEAD)
    }

    lang {
      name    = "go"
      version = "1.25"
    }
  }
}

deployment "shop" "worker" {
  replicas = 3

  env = {
    QUEUE = "orders"
  }

  build {
    context = "apps/worker"
    path    = "cmd/worker"

    lang {
      name    = "go"
      version = "1.25"
    }
  }
}

deployment "shop" "scheduler" {
  replicas = 1

  build {
    context = "apps/scheduler"
    path    = "cmd/scheduler"

    lang {
      name    = "go"
      version = "1.25"
    }
  }
}

ingress "shop" "api" {
  host = "api.shop.lvh.me"
  port = 8080

  // Empty tls {} = enabled + letsencrypt (the defaults). For
  // dev/staging that lacks public DNS, write `provider =
  // "internal"` so voodu issues its own self-signed certs.
  tls {}
}
