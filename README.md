<p align="center">
  <img src="docs/images/gokku-t-2.png" alt="Descrição da imagem" width="250">
</p>

# Gokku Deployment System

A **Docker-native** git-push deployment system for multi-language applications. No hard-coded app names, ports, or paths. Everything is configurable via `gokku.yml`.

**Gokku** = Go + Dokku - A lightweight alternative to Dokku, focused on containerized deployments with multi-language support.

<h1 style="z-index: 999; text-align: center;"></h1>

## Quick Start

Complete deployment in 4 steps:

### 1. Setup Server

From your local machine, run one-time command to setup everything:

```bash
gokku remote setup USER@SERVER_IP
```

This will:
- Install Gokku on the server
- Install essential plugins (nginx, letsencrypt, cron, postgres, redis)
- Configure SSH keys
- Verify installation
- Create default "gokku" remote for easy commands

### 2. Create App on Server

From your local machine (no SSH needed):

```bash
gokku apps create api-production --remote
```

### 3. Add Remote for Deployment

From your local machine:

```bash
gokku remote add api-production USER@SERVER_IP
```

### 4. Deploy

```bash
git push api-production main
```

That's it! Your app is live. 🎉

---

## Key Features

✅ **Docker-Native** - All applications run in containers with blue-green deployment

✅ **Zero Hard-coding** - Everything configured via `gokku.yml`

✅ **Multi-Language** - Go, Python, Node.js, Ruby (extensible)

✅ **Blue-Green Deployments** - Zero-downtime deployments with automatic rollback

✅ **CLI Management** - Comprehensive command-line interface for all operations

✅ **Config-Driven** - Apps, environments, ports all in config

✅ **Auto Dockerfile** - Generates Dockerfile if not exists

✅ **Health Checks** - Built-in health monitoring and restart policies  

---

## Configuration File (`gokku.yml`)

All project-specific settings are in one place.

### Minimal Configuration

Most fields are optional with sensible defaults:

```yaml
apps:
  api:
    path: ./cmd/api
    binary_name: api
```

This minimal config will use defaults:
- `lang: go` (default language)
- `environments: [production]` (default environment)
- `branch: main` (default branch for production)
- `deployment.keep_releases: 5` (default)
- `deployment.restart_policy: always` (default)

### Full Configuration Example

```yaml
apps:
  api-server:
    path: ./cmd/api
    binary_name: api-server
    workdir: .
    go_version: "1.25"
    environments:
      - name: production
        branch: main
        default_env_vars:
          LOG_LEVEL: info
      - name: staging
        branch: staging
        default_env_vars:
          LOG_LEVEL: debug
    deployment:
      keep_releases: 5
      restart_policy: always
      restart_delay: 5
    
  worker:
    path: ./cmd/worker
    binary_name: worker
    workdir: .
    go_version: "1.25"
    environments:
      - name: production
        branch: main
    deployment:
      keep_releases: 3
      restart_policy: always
      restart_delay: 5
    
  ml-service:
    lang: python
    path: ./services/ml
    dockerfile: ./services/ml/Dockerfile  # optional
    entrypoint: main.py
    image: "python:3.11-slim"

port_strategy: manual  # or 'auto' for sequential ports

docker:
  registry: ""  # empty = local, or docker.io, ghcr.io
  base_images:
    go: "golang:1.25-alpine"
    python: "python:3.11-slim"
    nodejs: "node:20-alpine"
```

---

## Configuration Defaults

All configuration fields are optional with sensible defaults:

### Build Configuration

| Field | Default | Description |
|-------|---------|-------------|
| `lang` | `go` | Programming language |
| `workdir` | `.` | Working directory for build |
| `go_version` | `1.25` | Go version (for Go apps) |
| `entrypoint` | `main.py` (Python)<br>`index.js` (Node.js) | Application entrypoint |
| `image` | ❌ No | Auto-detected | Docker base image or pre-built registry image |

### Image Configuration

The `image` field supports two modes:

**Base Image (Local Build):**
```yaml
image: "python:3.11-slim"  # Base image for local build
path: ./app
```

**Pre-built Registry Image (Ultra-fast Deployment):**
```yaml
image: "ghcr.io/meu-org/api:latest"  # Pre-built image from registry
```

When using a registry image (ghcr.io, ECR, docker.io, etc.), Gokku will:
1. Pull the pre-built image from the registry
2. Tag it for the application
3. Deploy directly (no build step required)

This enables ultra-fast deployments and integrates perfectly with CI/CD pipelines.

### Automatic Version Detection

When `build.image` is not specified, Gokku automatically detects the version from project files:

**Ruby:**
- `.ruby-version` file (e.g., `3.2.0`)
- `Gemfile` (e.g., `ruby '3.1.0'`)
- Fallback: `ruby:latest`

**Go:**
- `go.mod` file (e.g., `go 1.21`)
- Fallback: `golang:latest-alpine`

**Node.js:**
- `.nvmrc` file (e.g., `18.17.0`)
- `package.json` engines field (e.g., `"node": ">=18.0.0"`)
- Fallback: `node:latest`

**Python:**
- Always uses `python:latest` as fallback

### Environment Configuration

| Field | Default | Description |
|-------|---------|-------------|
| `environments` | `[production]` | List of environments |
| `branch` | `main` (production)<br>`staging` (staging)<br>`develop` (dev) | Git branch for environment |

### Deployment Configuration

| Field | Default | Description |
|-------|---------|-------------|
| `deployment.keep_releases` | `5` | Number of releases to keep |
| `deployment.keep_images` | `5` | Number of Docker images to keep |
| `deployment.restart_policy` | `always` | Docker restart policy |
| `deployment.restart_delay` | `5` | Restart delay in seconds |

### Examples

**Minimal Go app (all defaults):**
```yaml
apps:
  api:
    path: ./cmd/api
     binary_name: api
```

**Minimal Python app:**
```yaml
apps:
  ml-service:
    lang: python
    path: ./services/ml
```

**Custom everything:**
```yaml
apps:
  custom-app:
    path: ./cmd/custom
    binary_name: custom
    go_version: "1.24"
```

---

## Files Overview

### Core Files

- `gokku` - CLI binary for management
- `gokku.yml` - **Main configuration file**
- `hooks/` - Git hooks for automatic deployment

### Installers

- `install` - Universal installer (auto-detects server/client)

### Documentation

[https://gokku-vm.com/](https://gokku-vm.com/)

---

## Auto-Setup Feature

Gokku now features **automatic setup** on first deploy. No manual configuration required!

### How It Works

1. **First Push**: When you push to a new app/environment for the first time
2. **Auto-Detection**: Gokku detects it's a first deploy
3. **Config Reading**: Reads your `gokku.yml` from the repository
4. **Infrastructure Creation**: Automatically creates:
   - Git repository structure
   - Docker containers
   - Environment files
   - Directory structure
5. **Deploy**: Builds and deploys your application

### Benefits

- **Zero Manual Setup**: No need to run setup scripts
- **Configuration-Driven**: Uses your `gokku.yml` for all settings
- **Consistent**: Same setup process for all apps
- **Error-Free**: No manual steps to forget or get wrong

---

## Usage

### 1. Configure Your Project

Edit `gokku.yml` with your apps and environments:

```yaml
apps:
  api:
    build_path: ./cmd/api
    binary_name: api
    
  worker:
    build_path: ./cmd/worker
    binary_name: worker

environments:
  - name: production
    branch: main
  - name: staging
    branch: develop
```

### 2. Setup Server

The server setup is now **automatic**! No manual setup required.

Simply push your code and Gokku will:
- Detect it's the first deploy
- Read your `gokku.yml` configuration
- Create all necessary infrastructure
- Deploy your application

```bash
# Just push - setup happens automatically!
git push api-production main
```

### 3. Deploy

```bash
# First deploy - setup happens automatically
git push api-production main
git push worker-staging develop
```

### 4. Manage Applications

```bash
# List all applications
gokku apps

# Deploy applications
gokku deploy api-production
gokku deploy worker-staging

# Manage environment variables
gokku config set API_KEY=xxx -a api-production
gokku config list -a api-production

# View logs and status
gokku logs -a api-production
gokku status -a api-production

# Restart services
gokku restart -a api-production

# Rollback if needed
gokku rollback -a api-production
```

---

**Advantages:**
- Full control
- Clear and explicit
- Easy to document
- No conflicts

### Auto Strategy

Sequential ports assigned automatically:

```yaml
port_strategy: auto
base_port: 8000
```

Results in:
- `api-production` → 8000
- `api-staging` → 8001
- `worker-production` → 8002
- `worker-staging` → 8003

**Advantages:**
- No manual assignment
- Predictable
- Quick setup

---

## Docker Support

### Docker-Native Architecture

All applications run in Docker containers with blue-green deployment for zero-downtime updates:

```yaml
apps:
  api:
    path: ./cmd/api
    binary_name: api
    go_version: "1.25"
    goos: linux
    goarch: amd64
    cgo_enabled: 0
    
  ml-service:
    lang: python
    path: ./services/ml
    entrypoint: main.py
    image: "python:3.11-slim"
```

### Automatic Dockerfile Generation


**Go Apps:**
```dockerfile
FROM golang:1.25-alpine AS builder
WORKDIR /app
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o app ./cmd/api

FROM alpine:latest
RUN apk --no-cache add ca-certificates tzdata
WORKDIR /root/
COPY --from=builder /app/app .
EXPOSE ${PORT:-8080}
CMD ["./app"]
```

**Python Apps:**
```dockerfile
FROM python:3.11-slim
WORKDIR /app
RUN apt-get update && apt-get install -y gcc && rm -rf /var/lib/apt/lists/*
COPY requirements.txt* ./
RUN pip install --no-cache-dir -r requirements.txt
COPY . .
EXPOSE ${PORT:-8080}
CMD ["python", "main.py"]
```

### Custom Dockerfile

Specify your own Dockerfile in the build config:

```yaml
apps:
  ml-service:
    lang: python
    path: ./services/ml
      dockerfile: ./services/ml/Dockerfile  # Use this instead of auto-generation
    entrypoint: main.py
```

### How Docker Deployment Works

1. **Push code**: `git push ml-service-prod main`
2. **Hook extracts code** to release directory
3. **Checks for Dockerfile**:
   - If exists: uses it
   - If not: generates based on `lang`
4. **Builds image**: `ml-service:20250115-150405`
5. **Tags as latest**: `ml-service:latest`
6. **Blue-Green Deployment**:
   - Starts new container (green)
   - Health checks new container
   - Switches traffic from blue to green
   - Stops old container (blue)
7. **Cleanup old images** (keeps last 5)

### Rollback with Docker

Images are tagged with timestamps:

```bash
# Current
ml-service:latest → ml-service:20250115-150405

# Previous releases
ml-service:20250115-140305
ml-service:20250115-130200

# Rollback (manual for now)
docker stop ml-service-production
docker run --name ml-service-production ml-service:20250115-140305
```

### Multi-Language Deployments

All applications use Docker containers, regardless of language:

```yaml
apps:
  api:
    path: ./cmd/api
    binary_name: api
    
  worker:
    path: ./cmd/worker
    binary_name: worker
    
  vad:
    lang: python
    path: ./services/vad
    entrypoint: main.py
```

---

### Installation

```bash
# One-line install (future)
curl -fsSL https://gokku-vm.com/install | bash
```

### Benefits

1. **Reusable** - Use same deployer for all Go projects
2. **Versioned** - Update deployment system independently
3. **Shareable** - Teams can share deployment practices
4. **Maintainable** - One place to fix bugs/add features
5. **Clean** - Keep deployment separate from application code

---

## Quick Start

### Step 1: Create gokku.yml

Configure your project:

```yaml
apps:
  api:
    build_path: ./cmd/api
    binary_name: api
    
  worker:
    build_path: ./cmd/worker
    binary_name: worker

environments:
  - name: production
    branch: main
  - name: staging
    branch: staging

workdir: .  # or apps/trunk for your structure
```

### Step 2: Deploy (Auto-Setup)

```bash
# First push automatically sets up everything
git push api-production main
```

The first push will:
- Create the git repository
- Set up Docker containers
- Configure environment variables from `gokku.yml`
- Build and deploy your application

### Step 3: Manage Environment Variables

```bash
# Using gokku CLI
gokku config set PORT=8080 -a api-production
gokku config list -a api-production
```

---

## Configuration Reference

### Apps Section

```yaml
apps:
  app-name: 
    build_path: string     # Path to main package (relative to workdir)
    binary_name: string    # Output binary name (defaults to app name)
```

### Deployment Section

```yaml
deployment:
  keep_releases: number           # Number of releases to keep (default: 5)
  health_check_timeout: number    # Seconds to wait for health check
  restart_policy: string          # Docker restart policy (default: unless-stopped)
  restart_delay: number           # Seconds between restarts (default: 5)
```

### User Configuration

User configuration is **automatically detected** from your git remote URL.

**Example:**
```bash
# Git remote format: user@host:path
git remote add production ubuntu@server:api
# The user 'ubuntu' is automatically extracted and used
```

**No configuration needed** - Gokku automatically uses the user from your git remote.

---

### Application Variables

Set via env-manager for each app/environment:

```bash
env-manager --app api --env production set \
  API_KEY=xxx \
  DATABASE_URL=postgres://... \
  PORT=8080
```

---

## Advantages

| Aspect | This System |
|--------|-------------|
| **Reusability** | ✅ Any Go project |
| **Portability** | ✅ Separate repo ready |
| **Configuration** | ✅ Edit YAML |
| **New App** | ✅ Add to config |
| **New Environment** | ✅ Add to config |
| **Documentation** | ✅ In config file |
| **Validation** | ✅ Automatic |
| **Errors** | ✅ Early detection |

---

## Future Enhancements

### Planned Features

1. **Binary Distribution**
   - Pre-compiled binaries for Linux/macOS
   - `curl -fsSL https://... | sh` installer
   - GitHub releases with checksums

2. **Config Validation**
   - JSON Schema for gokku.yml
   - Linter for common mistakes
   - Best practices checker

3. **Multi-Language Support**
   - Node.js applications
   - Python applications
   - Any compiled language

4. **Advanced Features**
   - Blue-green deployments
   - Canary releases
   - Load balancer integration
   - Health check endpoints
   - Metrics collection

5. **Cloud Integrations**
   - AWS Systems Manager
   - Parameter Store for secrets
   - CloudWatch Logs
   - Auto-scaling groups

---

## Testing

### Test on Fresh EC2

```bash
# 1. Create fresh Ubuntu EC2
# 2. Install Gokku
curl -fsSL https://gokku-vm.com/install | bash

# 3. Create test project locally
mkdir test-project && cd test-project
git init

# 4. Create test config
cat > gokku.yml << EOF
apps:
  test-app:
    path: ./main.go
    binary_name: test-app
EOF

# 5. Create simple Go app
cat > main.go << EOF
package main
import "fmt"
func main() { fmt.Println("Hello Gokku!") }
EOF

# 6. Add git remote and push (auto-setup happens)
git add .
git commit -m "Initial commit"
git remote add production ubuntu@ec2:test-app
git push production main

# 7. Verify
ssh ubuntu@ec2 "docker ps | grep test-app"
```

---

## Contributing

Once you've made your great commits (include tests, please):

1. Fork this repository
2. Create a topic branch - git checkout -b my_branch
3. Push to your branch - git push origin my_branch
4. Create a pull request
5. That's it!

Please respect the indentation rules and code style. And use 2 spaces, not tabs. And don't touch the version thing or distribution files; this will be made when a new version is going to be release

---

## License

The Dockerfile and associated scripts and documentation in this project are released under the [MIT License](LICENSE).
