# Deployment

Learn how to deploy your applications with Gokku.

## Overview

Gokku uses a **git-push deployment** workflow:

1. Push code to Git remote on server
2. Git hook triggers automatically
3. Code is built and deployed
4. Service restarts with new version

## First Deployment

### 1. Create gokku.yml

In your project root:

```yaml
apps:
  api:
    path: ./cmd/api
```

### 2. Add Git Remote

On your local machine:

```bash
gokku remote add api-production ubuntu@server
```

### 3. Deploy (Auto-Setup)

```bash
git push api-production main
```

The first push automatically creates:
- Git repository
- App directories
- Docker containers
- Environment file from `gokku.yml`

Watch the deployment:

```
Counting objects: 100, done.
-----> Deploying api to production...
-----> Extracting code to /opt/gokku/apps/api/production/releases/1
-----> Building api...
-----> Build complete (binary: 5.2M)
-----> Creating symlink: current -> releases/1
-----> Restarting api-production...
-----> Deploy successful!
To ubuntu@server:api
   abc1234..def5678  main -> main
```

### 5. Verify

```bash
# Check container status
ssh ubuntu@server "docker ps | grep api"

# Check logs
ssh ubuntu@server "docker logs -f api"

# Test endpoint
curl http://your-server:8080/health
```

## Deployment Flow

### Docker Apps

```mermaid
graph TD
    A[git push] --> B[Git Hook Triggered]
    B --> C[Extract Code]
    C --> D{.tool-versions?}
    D -->|Yes| E[Generate Dockerfile]
    D -->|No| F{Custom Dockerfile?}
    E --> G[Build Image]
    F -->|Yes| G
    F -->|No| H[Generate Dockerfile]
    H --> G
    G --> I[Tag Image]
    I --> J[Stop Old Container]
    J --> K[Start New Container]
    K --> L[Cleanup Old Images]
    L --> M[Deploy Complete]
```

## Directory Structure

### Docker Deployment

```
/opt/gokku/apps/api/production/
├── .env            # Environment variables
├── deploy.log      # Deployment logs
└── (Docker images stored in Docker)
```

## Deployment Settings

Configure in `gokku.yml`:

```yaml
apps:
  api:
    deployment:
      keep_releases: 10       # Number of releases to keep
      keep_images: 10         # Number of Docker images to keep
      restart_policy: always  # Restart policy
      restart_delay: 5        # Seconds between restarts
```

### Restart Policies

- `always` - Always restart on failure (default)
- `on-failure` - Restart only on non-zero exit
- `no` - Never restart

## Atomic Deployments

Gokku uses **atomic deployments** for zero-downtime:

### How It Works

1. **Build in new directory** - New release doesn't affect running app
2. **Update symlink atomically** - Instant switch to new version
3. **Restart service** - Picks up new code

```bash
# Old version running
current -> releases/3

# New version built
releases/4/  (ready)

# Atomic switch
current -> releases/4  (instant)

# Restart picks up new code
docker restart api
```

### Downtime

- **Standard Docker**: ~2-5 seconds during container swap
- **Blue-Green (ZERO_DOWNTIME=1)**: Zero downtime with instant traffic switch

Add remotes:

```bash
git remote add production ubuntu@server:api
git remote add staging ubuntu@server:api
```

### Deploy

```bash
# Deploy to staging
git push staging staging

# Test staging
curl https://staging.example.com/health

# Deploy to production
git push production main
```

## Deployment Strategies

### 1. Direct to Production

Simple and fast:

```bash
git push production main
```

**Pros:**
- Fast
- Simple

**Cons:**
- No testing
- Risky

**Use when:**
- Small projects
- Solo developer
- Low traffic

### 2. Staging → Production

Test before production:

```bash
# 1. Deploy to staging
git push api-staging main

# 2. Test
./test-staging.sh

# 3. Deploy to production
git push api-production main
```

**Pros:**
- Catch bugs early
- Safe

**Cons:**
- Slower
- More setup

**Use when:**
- Team projects
- Critical apps
- Customer-facing

### 3. Feature Branches

Deploy branches to test:

```bash
# Deploy feature branch to staging
git push api-staging feature/new-ui

# Test
curl https://staging.example.com

# Merge and deploy to production
git checkout main
git merge feature/new-ui
git push api-production main
```

## Next Steps

- [Rollback](/guide/rollback) - Revert failed deployments
- [Environment Variables](/guide/env-vars) - Configure apps
- [Docker Support](/guide/docker) - Deploy with Docker
- [Examples](/examples/) - Real-world examples

