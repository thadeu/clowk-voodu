# Registry Deployment Examples

This guide shows how to use pre-built images from registries for ultra-fast deployments.

## Overview

Gokku supports two deployment modes:

1. **Local Build**: Build from source using base images
2. **Registry Pull**: Deploy pre-built images from registries (ghcr.io, ECR, etc.)

## Registry Deployment Examples

### GitHub Container Registry (ghcr.io)

```yaml
# gokku.yml
apps:
  api:
    image: "ghcr.io/meu-org/api:latest"
    deployment:
      keep_releases: 3
      restart_policy: always
```

### Amazon ECR

```yaml
apps:
  worker:
    image: "123456789012.dkr.ecr.us-east-1.amazonaws.com/meu-org/worker:latest"
    deployment:
      keep_releases: 5
      restart_policy: always
```

### Docker Hub

```yaml
apps:
  web:
    image: "meu-org/web:latest"
    ports:
      - "80:8080"
    deployment:
      keep_releases: 3
      restart_policy: always
```

## CI/CD Integration

### GitHub Actions Example

```yaml
# .github/workflows/build-and-push.yml
name: Build and Push

on:
  push:
    branches: [main]

jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v3
      
      - name: Build Docker image
        run: |
          docker build -t ghcr.io/meu-org/api:${{ github.sha }} .
          docker push ghcr.io/meu-org/api:${{ github.sha }}
          
      - name: Deploy to Gokku
        run: |
          # Update gokku.yml with new image tag
          sed -i "s|ghcr.io/meu-org/api:latest|ghcr.io/meu-org/api:${{ github.sha }}|" gokku.yml
          gokku deploy
```

### GitLab CI Example

```yaml
# .gitlab-ci.yml
build:
  stage: build
  script:
    - docker build -t registry.gitlab.com/meu-org/api:$CI_COMMIT_SHA .
    - docker push registry.gitlab.com/meu-org/api:$CI_COMMIT_SHA
  only:
    - main

deploy:
  stage: deploy
  script:
    - sed -i "s|registry.gitlab.com/meu-org/api:latest|registry.gitlab.com/meu-org/api:$CI_COMMIT_SHA|" gokku.yml
    - gokku deploy
  only:
    - main
```

## Mixed Deployment Strategy

You can mix both approaches in the same project:

```yaml
# gokku.yml
apps:
  # Pre-built image (fast deployment)
  api:
    image: "ghcr.io/meu-org/api:latest"
      
  # Local build (development/testing)
  worker:
    lang: python
    image: "python:3.11-slim"  # Base image
      path: ./worker
```

## Benefits

### Registry Deployment
- **Ultra-fast**: No build time, just pull and deploy
- **CI/CD Integration**: Build once, deploy anywhere
- **Consistency**: Same image across environments
- **Scalability**: Perfect for production workloads

### Local Build
- **Development**: Great for local development and testing
- **Flexibility**: Easy to modify and iterate
- **Simplicity**: No need for external registries

## Best Practices

1. **Use registries for production**: Pre-built images are faster and more reliable
2. **Use local builds for development**: Faster iteration during development
3. **Tag your images**: Use semantic versioning or commit hashes
4. **Clean up old images**: Configure `keep_images` to manage storage
5. **Use private registries**: For sensitive applications

## Registry Authentication

### GitHub Container Registry

```bash
# Login to ghcr.io
echo $GITHUB_TOKEN | docker login ghcr.io -u USERNAME --password-stdin
```

### Amazon ECR

```bash
# Login to ECR
aws ecr get-login-password --region us-east-1 | docker login --username AWS --password-stdin 123456789012.dkr.ecr.us-east-1.amazonaws.com
```

### Docker Hub

```bash
# Login to Docker Hub
docker login
```

## Custom Registry Configuration

### Adding Custom Registries

If you use private or custom registries that aren't automatically detected, you can configure them in your `gokku.yml`:

```yaml
# gokku.yml
docker:
  registry:  # List of custom registries
    - "self-ghrc.io"  # Your own GitHub Container Registry
    - "registry.company.com"  # Company private registry
    - "harbor.example.com"  # Harbor registry
```

### Examples with Custom Registries

```yaml
# Using custom company registry
apps:
  internal-api:
    image: "registry.company.com/meu-org/api:latest"

# Using Harbor registry
apps:
  microservice:
    image: "harbor.example.com/project/service:latest"
```

## Troubleshooting

### Image Pull Failures

```bash
# Check if image exists
docker pull ghcr.io/meu-org/api:latest

# Check registry authentication
docker login ghcr.io
```

### Build vs Registry Detection

Gokku automatically detects registry images by checking for common registry patterns:
- `ghcr.io/`
- `docker.io/`
- `quay.io/`
- `gcr.io/`
- `amazonaws.com/`
- `azurecr.io/`
- And many more...

**Custom registries** are detected from your `gokku.yml` configuration:
```yaml
docker:
  registry:
    - "your-custom-registry.com"
    - "another-registry.com"
```

If your registry isn't detected, you can add it to the `docker.registry` list in your `gokku.yml`.
