# Environment Variables

Manage environment variables for your applications with Gokku.

## Overview

Gokku provides a unified CLI tool for managing environment variables, similar to Heroku's `config:set`, accessible both locally on the server and remotely from your machine.

## gokku config

The `gokku config` command manages environment variables.

### Usage Modes

**Remote execution (from local machine):**
```bash
gokku config <command> [args] -a <app-name>
```

**Local execution (on server):**
```bash
gokku config <command> [args] --app <app-name>
```

## Commands

### set - Set Variable

Set environment variable(s):

**From local machine:**
```bash
gokku config set PORT=8080 -a api-production
gokku config set DATABASE_URL="postgres://..." -a api-production
```

**On server:**
```bash
gokku config set PORT=8080 --app api
gokku config set DATABASE_URL="postgres://user:pass@localhost/db" --app api
```

**Multiple variables:**

```bash
gokku config set PORT=8080 -a api-production
gokku config set LOG_LEVEL=info -a api-production
gokku config set WORKERS=4 -a api-production
```

### get - Get Variable

Get a variable value:

**From local machine:**
```bash
gokku config get PORT -a api-production
```

**On server:**
```bash
gokku config get PORT --app api
```

Output:
```
PORT=8080
```

### list - List All Variables

List all environment variables:

**From local machine:**
```bash
gokku config list -a api-production
```

**On server:**
```bash
gokku config list --app api
```

Output:
```
DATABASE_URL=postgres://user:pass@localhost/db
LOG_LEVEL=info
PORT=8080
WORKERS=4
```

### unset - Delete Variable

Delete an environment variable:

**From local machine:**
```bash
gokku config unset PORT -a api-production
```

**On server:**
```bash
gokku config unset PORT --app api
```

## Storage

Environment variables are stored in:

```
/opt/gokku/apps/APP_NAME/shared/.env
```

Example: `/opt/gokku/apps/api/shared/.env`

### Format

Standard `.env` format:

```env
DATABASE_URL=postgres://localhost/db
LOG_LEVEL=info
PORT=8080
REDIS_URL=redis://localhost:6379
```

### How It Works

- Environment variables are stored in a single shared `.env` file per application
- Each release directory gets a symlink to this shared file
- Changes to environment variables affect all releases immediately
- No restart is needed for environment variable changes to take effect

### Access from Application

**Go:**
```go
import "os"

port := os.Getenv("PORT")
dbURL := os.Getenv("DATABASE_URL")
```

**Python:**
```python
import os

port = os.getenv("PORT", "8080")
db_url = os.getenv("DATABASE_URL")
```

**Node.js:**
```javascript
const port = process.env.PORT || 8080;
const dbURL = process.env.DATABASE_URL;
```


## Apply Changes

Environment variables are automatically available to your application. No restart is required for environment variable changes to take effect.

### Restart Application (Optional)

If you want to restart the application after changing environment variables:

**From local machine:**
```bash
gokku restart -a api-production
```

**On server:**
```bash
gokku restart --app api
```

## Common Variables

### Server Configuration

```bash
# Port
gokku config set PORT=8080 -a api-production

# Workers/Threads
gokku config set WORKERS=4 -a api-production

# Timeout
gokku config set TIMEOUT=30 -a api-production
```

### Database

```bash
# PostgreSQL
gokku config set DATABASE_URL="postgres://user:pass@host:5432/dbname" -a api-production

# MySQL
gokku config set DATABASE_URL="mysql://user:pass@host:3306/dbname" -a api-production

# MongoDB
gokku config set MONGO_URL="mongodb://user:pass@host:27017/dbname" -a api-production
```

### Cache

```bash
# Redis
gokku config set REDIS_URL="redis://localhost:6379" -a api-production

# Memcached
gokku config set MEMCACHE_SERVERS="localhost:11211" -a api-production
```

### AWS

```bash
gokku config set AWS_ACCESS_KEY_ID="AKIA..." -a api-production
gokku config set AWS_SECRET_ACCESS_KEY="secret..." -a api-production
gokku config set AWS_REGION="us-east-1" -a api-production
gokku config set S3_BUCKET="my-bucket" -a api-production
```

### API Keys

```bash
gokku config set STRIPE_API_KEY="sk_live_..." -a api-production
gokku config set SENDGRID_API_KEY="SG...." -a api-production
gokku config set OPENAI_API_KEY="sk-..." -a api-production
```

### Application

```bash
# Environment
gokku config set APP_ENV=production -a api-production

# Debug mode
gokku config set DEBUG=false -a api-production

# Log level
gokku config set LOG_LEVEL=info -a api-production

# Secret key
gokku config set SECRET_KEY="random-secret-key" -a api-production
```

## Security Best Practices

✅ **Good:**
```bash
# Use gokku config instead
gokku config set DATABASE_URL="postgres://..." -a api-production
```

### 2. Use Strong Secrets

```bash
# Generate random secret
SECRET=$(openssl rand -hex 32)
gokku config set SECRET_KEY="$SECRET" -a api-production
```

### 3. Different Secrets Per Environment

```bash
# Production
gokku config set SECRET_KEY="prod-secret" -a api-production

# Staging
gokku config set SECRET_KEY="staging-secret" -a api-staging
```

### 4. Rotate Secrets Regularly

```bash
# Generate new secret
NEW_SECRET=$(openssl rand -hex 32)

# Update
gokku config set SECRET_KEY="$NEW_SECRET" -a api-production

# Restart app (optional)
gokku restart -a api-production
```

## Multiple Environments

Different variables for different environments:

```bash
# Production - Real database
gokku config set DATABASE_URL="postgres://prod-db/app" -a api-production
gokku config set LOG_LEVEL=info -a api-production
gokku config set DEBUG=false -a api-production

# Staging - Staging database
gokku config set DATABASE_URL="postgres://staging-db/app" -a api-staging
gokku config set LOG_LEVEL=debug -a api-staging
gokku config set DEBUG=true -a api-staging
```

## Troubleshooting

### Variable Not Found

Check if variable exists:

```bash
# From local machine
gokku config get PORT -a api-production

# On server
gokku config get PORT --app api
```

If empty, set it:

```bash
gokku config set PORT=8080 -a api-production
```

### Changes Not Applied

Environment variables are automatically available to your application. If you need to restart:

```bash
# Using gokku CLI
gokku restart -a api-production

# Or manually on server
gokku restart --app api
```

### .env File Missing

The `.env` file is created automatically when you first set a variable:

```bash
gokku config set PORT=8080 -a api-production
```

### Permission Denied

Fix permissions:

```bash
ssh ubuntu@server "sudo chown ubuntu:ubuntu /opt/gokku/apps/api/shared/.env"
ssh ubuntu@server "chmod 600 /opt/gokku/apps/api/shared/.env"
```

## Advanced Usage

### Bulk Set

Set multiple variables at once:

```bash
# Set multiple variables in one command
gokku config set PORT=8080 -a api-production
gokku config set LOG_LEVEL=info -a api-production
gokku config set WORKERS=4 -a api-production
```

### Backup Variables

```bash
# Backup
ssh ubuntu@server "cat /opt/gokku/apps/api/shared/.env" > backup.env

# Restore
scp backup.env ubuntu@server:/opt/gokku/apps/api/shared/.env
gokku restart -a api-production
```

### Export from Local .env

```bash
# Read local .env and set on server
while IFS= read -r line; do
  if [[ $line && $line != \#* ]]; then
    gokku config set "$line" -a api-production
  fi
done < .env.production
```

## Next Steps

- [Configuration](/guide/configuration) - Configure apps
- [Deployment](/guide/deployment) - Deploy your app
- [CLI Reference](/reference/cli) - Complete command reference
- [Troubleshooting](/reference/troubleshooting) - Common issues

