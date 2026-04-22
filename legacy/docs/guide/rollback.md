# Rollback

Quickly revert to a previous deployment when things go wrong.

## Overview

Gokku keeps multiple releases, allowing you to rollback instantly without redeploying.

## How It Works

### Release Management

Each deployment creates a new release directory with a timestamp:

```
/opt/gokku/apps/api/
├── releases/
│   ├── 20240115-100000/  ← First deploy
│   ├── 20240115-113000/  ← Second deploy
│   ├── 20240115-142000/  ← Third deploy
│   ├── 20240116-091500/  ← Fourth deploy
│   └── 20240116-104500/  ← Latest deploy
└── current -> releases/20240116-104500/  ← Symlink to active release
```

The `current` symlink points to the active release.

### Docker Images

For Docker applications, each deployment creates a tagged image:

```
api:release-20240115-100000
api:release-20240115-113000
api:release-20240115-142000
api:release-20240116-091500
api:release-20240116-104500  ← Latest
```

The container runs the latest tagged image.

## Automatic Cleanup

Gokku automatically keeps the last 5 releases and removes older ones during deployment. This helps manage disk space while keeping recent releases available for rollback.

## Using the Rollback Command

Gokku provides a built-in rollback command that handles the rollback process automatically.

### Basic Rollback

Rollback to the previous release:

```bash
# Remote execution
gokku rollback -a api-production

# Local execution (on server)
gokku rollback api production
```

### Rollback to Specific Release

Rollback to a specific release by providing the release ID:

```bash
# Remote execution
gokku rollback -a api-production 20240115-113000

# Local execution (on server)
gokku rollback api production 20240115-113000
```

### List Available Releases

To see available releases for rollback:

```bash
# SSH to server and list releases
ssh ubuntu@server "ls -la /opt/gokku/apps/api/releases/"

# Or check Docker images
ssh ubuntu@server "docker images | grep api"
```

### Verify Rollback

After rollback, verify the application is running:

```bash
# Check status
gokku status -a api-production

# Check logs
gokku logs -a api-production -f
```

## Quick Rollback Workflow

### 1. Deploy New Version

```bash
gokku deploy -a api-production
```

Output:
```
-----> Deploying api to production...
-----> Release 20240116-104500 deployed
```

### 2. Notice Issue

```bash
# Check logs
gokku logs -a api-production -f

# Test endpoint
curl https://api.example.com/health
# Error!
```

### 3. Rollback

```bash
# Rollback to previous release
gokku rollback -a api-production
```

### 4. Verify

```bash
# Check status
gokku status -a api-production

# Test endpoint
curl https://api.example.com/health
# OK!
```

### 5. Fix Issue Locally

```bash
# Fix the bug
git commit -am "fix: critical bug"

# Redeploy
gokku deploy -a api-production
```

## Rollback Best Practices

### 1. Test Before Rollback

Check if issue is deployment-related:

```bash
# Check service status
gokku status -a api-production

# Check logs
gokku logs -a api-production

# Test endpoint
curl https://api.example.com/health
```

### 2. Document Rollback

Keep track of rollbacks:

```bash
# Note the issue
echo "2024-01-16 10:50 - Rolled back from release 20240116-104500 to 20240115-113000 due to database migration issue" >> rollback.log
```

### 3. Investigate Root Cause

After rollback, find and fix the issue:

```bash
# Check deployment logs
gokku logs -a api-production

# Review changes in your local repository
git log --oneline -10
```

### 4. Don't Delete Failed Release

Gokku automatically manages old releases, keeping the last 5. Failed releases are automatically cleaned up after 5 successful deployments.

### 5. Test Staging First

Deploy to staging before production:

```bash
# Deploy to staging
gokku deploy -a api-staging

# Test thoroughly
curl https://staging.example.com/health

# Then deploy to production
gokku deploy -a api-production
```

## Database Migrations

### Important Consideration

Rolling back code doesn't automatically rollback database changes:

```
Release 20240116-104500: Add column 'email' to users
Release 20240115-113000: Code doesn't know about 'email' column
```

Rollback to previous release → Code expects old schema!

### Best Practices

#### 1. Backward Compatible Migrations

Always write backward-compatible migrations:

```sql
-- Good: Add column with default
ALTER TABLE users ADD COLUMN email VARCHAR(255) DEFAULT '';

-- Bad: Add required column
ALTER TABLE users ADD COLUMN email VARCHAR(255) NOT NULL;
```

#### 2. Two-Phase Migrations

**Phase 1:** Add column (deploy new release)
```sql
ALTER TABLE users ADD COLUMN email VARCHAR(255);
```

**Phase 2:** Make required (deploy next release)
```sql
ALTER TABLE users ALTER COLUMN email SET NOT NULL;
```

Now rollback from latest to previous release is safe.

#### 3. Manual Database Rollback

If you must rollback database changes:

```bash
# 1. Rollback code
gokku rollback -a api-production

# 2. Rollback database manually (if needed)
gokku run bundle exec rails db:rollback -a api-production
# or
gokku run "python manage.py migrate app_name 0001" -a api-production
```

## Environment Variables

### After Rollback

Environment variables are shared across releases, so they remain consistent after rollback:

```bash
# List current vars
gokku config list -a api-production

# Environment variables are stored in shared/.env
# and linked to each release directory
```

## Partial Rollback

### Rollback One App in Monorepo

If deploying multiple apps:

```bash
# Rollback only API
gokku rollback -a api-production

# Keep worker on latest
# (no changes)
```

### Rollback Environment Only

Rollback staging but not production:

```bash
# Rollback staging
gokku rollback -a api-staging

# Production stays on latest
```


## Troubleshooting

### Container Won't Start

```
docker: Error response from daemon: Conflict
```

**Fix:**
```bash
# Check container status
gokku status -a api-production

# Check logs for errors
gokku logs -a api-production

# If needed, restart the application
gokku restart -a api-production
```

### Release Not Found

```
Error: No previous release found
```

**Fix:**

Release was cleaned up. Check available releases:

```bash
# List available releases
ssh ubuntu@server "ls /opt/gokku/apps/api/releases/"

# Or check Docker images
ssh ubuntu@server "docker images | grep api"
```

Rollback to an existing one using the specific release ID.

### Application Won't Start After Rollback

```bash
# Check status
gokku status -a api-production

# Check logs
gokku logs -a api-production

# If needed, restart
gokku restart -a api-production
```

## Monitoring Rollbacks

### Track Rollbacks

Keep a log of rollbacks for debugging:

```bash
# Create rollback log
echo "$(date) - Rollback api-production: 20240116-104500 -> 20240115-113000" >> rollback.log

# View history
cat rollback.log
```

## Next Steps

- [Deployment](/guide/deployment) - Deployment strategies
- [Environment Variables](/guide/env-vars) - Manage configs
- [CLI Reference](/reference/cli) - Complete command reference

