# Docker Support

Deploy applications using Docker containers with Gokku.

## Overview

Gokku uses Docker containers for all applications with blue-green deployment for zero-downtime updates.

All applications run in Docker containers regardless of language:
- **Go** - Compiled in container
- **Python** - Interpreted in container  
- **Node.js** - Interpreted in container
- **Ruby** - Interpreted in container

## Configuration

All applications use Docker by default. Configure your app:

```yaml
apps:
  my-app:
    lang: python
    path: ./app
    entrypoint: main.py
```

## Automatic Dockerfile Generation

If no Dockerfile exists, Gokku generates one based on your language:

### Python

```yaml
apps:
  flask-app:
    lang: python
    path: .
    entrypoint: app.py
```

Generated Dockerfile:
```dockerfile
FROM python:3.11-slim

WORKDIR /app

RUN apt-get update && apt-get install -y --no-install-recommends gcc && rm -rf /var/lib/apt/lists/*

COPY requirements.txt* ./
RUN if [ -f requirements.txt ]; then pip install --no-cache-dir -r requirements.txt; fi

COPY . .

EXPOSE ${PORT:-8080}

CMD ["python", "app.py"]
```

### Node.js

```yaml
apps:
  node-app:
    lang: nodejs
    path: .
    entrypoint: index.js
```

Generated Dockerfile:
```dockerfile
FROM node:20-alpine

WORKDIR /app

COPY package*.json ./
RUN npm ci --only=production

COPY . .

EXPOSE ${PORT:-8080}

CMD ["node", "index.js"]
```

### Go (with Docker)

```yaml
apps:
  go-app:
    path: .
```

Generated Dockerfile:
```dockerfile
FROM golang:1.25-alpine AS builder

WORKDIR /app
COPY go.* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o app .

FROM alpine:latest
RUN apk --no-cache add ca-certificates
WORKDIR /root/
COPY --from=builder /app/app .

EXPOSE ${PORT:-8080}

CMD ["./app"]
```

## Custom Dockerfile

Use your own Dockerfile:

```yaml
apps:
  my-app:
    dockerfile: ./Dockerfile
```

### Example: Python with System Dependencies

```dockerfile
FROM python:3.11-slim

# Install system packages
RUN apt-get update && apt-get install -y \
    gcc \
    ffmpeg \
    libpq-dev \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

COPY . .

EXPOSE ${PORT:-8080}

CMD ["gunicorn", "--bind", "0.0.0.0:${PORT:-8080}", "app:app"]
```

### Example: Multi-Stage Build

```dockerfile
# Build stage
FROM node:20-alpine AS builder
WORKDIR /app
COPY package*.json ./
RUN npm ci
COPY . .
RUN npm run build

# Production stage
FROM node:20-alpine
WORKDIR /app
COPY package*.json ./
RUN npm ci --only=production
COPY --from=builder /app/dist ./dist

EXPOSE ${PORT:-8080}

CMD ["node", "dist/server.js"]
```

## Base Images

### Configure Global Base Images

```yaml
docker:
  base_images:
    go: "golang:1.25-alpine"
    python: "python:3.11-slim"
    nodejs: "node:20-alpine"
```

### Per-App Base Image

```yaml
apps:
  ml-service:
    lang: python
    image: "python:3.11"  # Full image, not slim
```

### From Private Registry

```yaml
docker:
  registry: "registry.example.com"

apps:
  api:
    image: "registry.example.com/python:3.11-custom"
```

Login on server first:
```bash
ssh ubuntu@server "docker login registry.example.com"
```

## Custom Dockerfiles

Gokku supports custom Dockerfiles for advanced use cases.

```
python 3.11
ffmpeg 8.0
```

Gokku generates:

```dockerfile
FROM python:3.11-slim

WORKDIR /app

# Install dependencies
RUN apt-get update && apt-get install -y curl git build-essential && rm -rf /var/lib/apt/lists/*

# Copy application
COPY . .

EXPOSE ${PORT:-8080}

CMD ["python", "app.py"]
```

`.tool-versions`:
```
python 3.11
ffmpeg 8.0
whispercpp 1.5.0
```

Generated Dockerfile includes plugin installation:

```dockerfile
# Install custom dependencies
RUN apt-get update && apt-get install -y ffmpeg
# ...
```

## Image Management

### Tagging

Gokku automatically tags images:

```
my-app:release-1
my-app:release-2
my-app:release-3
```

Each deployment creates a new tag.

### Keep Images

Configure how many images to keep:

```yaml
apps:
  my-app:
    deployment:
      keep_images: 10  # Keep last 10 images
```

Old images are automatically pruned after deployment.

### Manual Cleanup

```bash
# List images
ssh ubuntu@server "docker images | grep my-app"

# Remove specific image
ssh ubuntu@server "docker rmi my-app:release-5"

# Remove all unused images
ssh ubuntu@server "docker image prune -a"
```

## Port Mapping

Docker maps: `8080:8080`

### Custom Ports

Set via environment variables:

```bash
gokku config set PORT=8081 --app api --env production -a api-production
```

Then redeploy.

## Container Management

### View Running Containers

```bash
ssh ubuntu@server "docker ps | grep my-app"
```

### View Logs

```bash
# Using CLI
gokku logs -a my-app-production -f

# Or directly
ssh ubuntu@server "docker logs -f my-app-blue"
```

### Restart Container

```bash
# Using CLI
gokku restart -a my-app-production

# Or directly
ssh ubuntu@server "docker restart my-app-blue"
```

### Stop Container

```bash
ssh ubuntu@server "docker stop my-app-blue"
```

### Start Container

```bash
ssh ubuntu@server "docker start my-app-blue"
```

### Access Container Shell

```bash
ssh ubuntu@server "docker exec -it my-app-blue /bin/sh"
```

### Inspect Container

```bash
ssh ubuntu@server "docker inspect my-app-blue"
```

## Environment Variables

All environment variables are passed to the container:

```bash
# Set variable
gokku config set DATABASE_URL=postgres://... -a api-production

# Restart container to pick up changes
gokku restart -a api-production
```

## Health Checks

Add health check to Dockerfile:

```dockerfile
FROM python:3.11-slim

WORKDIR /app
COPY . .

EXPOSE ${PORT:-8080}

# Health check
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD python -c "import urllib.request; urllib.request.urlopen('http://localhost:${PORT:-8080}/health')" || exit 1

CMD ["python", "app.py"]
```

Check health status:

```bash
ssh ubuntu@server "docker inspect --format='{{.State.Health.Status}}' api-blue"
```

## Volumes (Limited Support)

Gokku doesn't currently support Docker volumes in config. Workarounds:

### Option 2: Host Bind Mount (Manual)

Modify the hook template (advanced users):

```bash
docker run -d \
  --name $APP_NAME-blue \
  -v /data/uploads:/app/uploads \
  -p $PORT:$PORT \
  $IMAGE_NAME
```

### Option 3: Separate Data Container

Run database/redis outside Gokku:

```bash
# On server
docker run -d \
  --name postgres \
  -v /data/postgres:/var/lib/postgresql/data \
  -p 5432:5432 \
  postgres:15
```

## Networking

### Container to Container

Use Docker host IP (`172.17.0.1`) or `host.docker.internal`.

## Blue-Green Deployment

Gokku uses blue-green deployment for zero-downtime updates:

### How It Works

1. **New Deployment**: Code is deployed to the green environment
2. **Health Check**: System verifies the green environment is healthy
3. **Traffic Switch**: Traffic is switched from blue to green
4. **Cleanup**: Old blue environment is stopped and cleaned up

### Enable Zero-Downtime

```bash
gokku config set ZERO_DOWNTIME=true -a <app>-<env>
```

### Container Naming

- **Active Container**: `<app-name>-blue`
- **New Container**: `<app-name>-green`

For more details, see [Blue-Green Deployment Guide](/guide/blue-green-deployment).

## Troubleshooting

### Build Failed

Check deploy logs:

```bash
ssh ubuntu@server "cat /opt/gokku/apps/my-app/production/deploy.log"
```

### Container Won't Start

Check Docker logs:

```bash
ssh ubuntu@server "docker logs my-app-blue"
```

### Out of Disk Space

Clean up old images:

```bash
ssh ubuntu@server "docker system prune -a"
```

### Image Pull Failed

Check registry login:

```bash
ssh ubuntu@server "docker login registry.example.com"
```

## Advanced

### Custom Docker Run Options

To add custom Docker run options, modify the hook template:

`/opt/gokku/hooks/post-receive-docker.template`

Add options to the `docker run` command.

### Docker Compose (Not Supported)

Gokku uses `docker run`, not `docker-compose`. For multi-container apps:

1. Deploy each service separately
2. Or use a process manager inside one container (supervisord)

### Private Registry

```yaml
docker:
  registry: "registry.example.com"

apps:
  api:
    image: "registry.example.com/python:3.11"
```

Login on server:
```bash
ssh ubuntu@server "docker login registry.example.com"
```

## Next Steps

- [Environment Variables](/guide/env-vars) - Configure containers
- [Rollback](/guide/rollback) - Rollback to previous images
- [Examples](/examples/docker-app) - Real-world examples

