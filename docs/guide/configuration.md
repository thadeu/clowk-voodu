# Configuration

Gokku is configured via a `gokku.yml` file in your project root. This file defines your apps, environments, and deployment settings.

## Minimal Configuration

The simplest `gokku.yml`:

```yaml
apps:
  api:
    path: ./cmd/api
```

That's it! Everything else has sensible defaults.

## Full Configuration

Here's a complete example with all options:

```yaml
apps:
  api:
    path: ./cmd/api
    binary_name: api
    workdir: .
    go_version: "1.25"
    goos: linux
    goarch: amd64
    cgo_enabled: 0
    deployment:
      keep_releases: 5
      restart_policy: always
      restart_delay: 5
```

## Configuration Sections

### Apps

The main configuration section. Each app can have:

#### Basic Settings

```yaml
apps:
  api:           # App name (required)
    lang: go            # Language (optional, default: go)
```

#### Build Configuration

```yaml
path: ./cmd/api         # Path to main file/directory
workdir: .             # Working directory for build

# Go-specific settings
go_version: "1.25"      # Go version
goos: linux             # Target OS
goarch: amd64           # Target architecture
cgo_enabled: 0          # Enable CGO (0 or 1)

# Docker-specific settings
dockerfile: ./Dockerfile     # Custom Dockerfile path
      
```

**Build Type Defaults:**

| Setting | Default | Description |
|---------|---------|-------------|
| `path` | (required) | Build path |
| `workdir` | `.` | Working directory |
| `dockerfile` | `./Dockerfile` | Dockerfile path |
| `base_image` | (language-specific) | Base Docker image |


#### Deployment

Deployment settings:

```yaml
    deployment:
      keep_releases: 5              # Number of releases to keep
      keep_images: 5                # Number of Docker images to keep
      restart_policy: unless-stopped # Docker restart policy
      restart_delay: 5              # Delay between restarts (seconds)
```

**Defaults:**
- `keep_releases`: `5`
- `keep_images`: `5`
- `restart_policy`: `unless-stopped`
- `restart_delay`: `5`

### User Configuration

User configuration is **automatically detected** from your git remote URL.

**Example:**
```bash
# Git remote format: user@host:path
gokku remote add api-production ubuntu@server
# The user 'ubuntu' is automatically extracted and used
```

**No configuration needed** - Gokku automatically uses the user from your git remote.

## Configuration by Language

### Go Application

```yaml
apps:
  api:
    path: ./cmd/api
    go_version: "1.25"
```

### Python Application

```yaml
apps:
  worker:
    lang: python
    image: python:3.11-slim
    path: ./apps/worker
```

### Node.js Application

```yaml
apps:
  frontend:
    lang: nodejs
    image: node:20-alpine
    path: ./apps/frontend
```

## Multi-App Configuration

Deploy multiple apps from one repository:

```yaml
apps:
  api:
    path: ./cmd/api
  
  worker:
    path: ./cmd/worker
  
  ml-service:
    path: ./services/ml
    entrypoint: server.py
```

Each app gets:
- Separate Git repository
- Independent environments
- Isolated deployments

## Validation

Gokku validates your configuration on deployment. Common errors:

### Missing Required Fields

```
Error: app 'api' missing required field: build.path
```

**Fix:** Add `build.path`:

```yaml
apps:
  api:
    path: ./cmd/api  # Required!
```

### 2. Version Control

Always commit `gokku.yml`:

```bash
git add gokku.yml
git commit -m "feat: add gokku configuration"
```

### 3. Document Custom Settings

Add comments for non-obvious settings:

```yaml
apps:
  api:
    cgo_enabled: 1  # Required for SQLite
```

## Examples

See [Examples](/examples/) for real-world configurations:

- [Go Application](/examples/go-app)
- [Python Application](/examples/python-app)
- [Docker Application](/examples/docker-app)
- [Multi-App Project](/examples/multi-app)

## Reference

Full configuration reference: [Configuration Reference](/reference/configuration)

