# Blue-Green Deployment

Gokku uses blue-green deployment for zero-downtime updates of your applications.

## How It Works

Blue-green deployment maintains two identical production environments:

- **Blue**: Currently active environment
- **Green**: New deployment environment

### Deployment Process

1. **New Deployment**: Code is deployed to the green environment
2. **Health Check**: System verifies the green environment is healthy
3. **Traffic Switch**: Traffic is switched from blue to green
4. **Cleanup**: Old blue environment is stopped and cleaned up

## Configuration

### Enable Zero-Downtime Deployment

Add to your application's environment file:

```bash
# Enable zero-downtime deployment
ZERO_DOWNTIME=true
```

### Environment File Location

Environment files are located at:
```
/opt/gokku/apps/<app-name>/shared/.env
```

### Set via CLI

```bash
# Enable zero-downtime deployment
gokku config set ZERO_DOWNTIME=true -a <app>-<env>

# Disable zero-downtime deployment
gokku config set ZERO_DOWNTIME=false -a <app>-<env>
```

## Container Naming

Gokku uses a consistent naming convention for containers:

- **Active Container**: `<app-name>-blue`
- **New Container**: `<app-name>-green`

### Example

For an app named `api`:
- Active: `api-blue`
- New deployment: `api-green`

## Deployment Flow

### 1. Initial Deployment

```bash
git push api-production main
```

**What happens:**
1. Code is extracted to release directory
2. Docker image is built
3. Green container is started
4. Health checks are performed
5. Green container becomes blue (active)
6. Old blue container is stopped

### 2. Subsequent Deployments

```bash
git push api-production main
```

**What happens:**
1. Code is extracted to release directory
2. Docker image is built
3. Green container is started
4. Health checks are performed
5. Traffic switches from blue to green
6. Old blue container is stopped and removed
7. Green container is renamed to blue

## Health Checks

Gokku performs health checks on new deployments:

### Default Health Check

- **Endpoint**: `/health`
- **Method**: GET
- **Expected Response**: HTTP 200

### Custom Health Check

Set a custom health check endpoint:

```bash
gokku config set HEALTH_CHECK_PATH=/api/health -a <app>-<env>
```

### Health Check Timeout

Configure health check timeout (default: 30 seconds):

```bash
gokku config set HEALTH_CHECK_TIMEOUT=60 -a <app>-<env>
```

## Monitoring Deployments

### Check Container Status

```bash
# List all containers
docker ps | grep <app-name>

# Check specific containers
docker ps | grep api-blue
docker ps | grep api-green
```

### View Deployment Logs

```bash
# View logs from active container
gokku logs -a <app>-<env> -f

# View logs from specific container
ssh ubuntu@server "docker logs -f <app-name>-blue"
ssh ubuntu@server "docker logs -f <app-name>-green"
```

### Check Deployment Status

```bash
# Check application status
gokku status -a <app>-<env>

# Check container health
ssh ubuntu@server "docker inspect <app-name>-blue"
```

## Rollback

### Automatic Rollback

If health checks fail, Gokku automatically:
1. Stops the green container
2. Keeps the blue container running
3. Reports deployment failure

### Manual Rollback

```bash
# Rollback to previous release
gokku rollback -a <app>-<env>

# Rollback to specific release
gokku rollback -a <app>-<env> <release-id>
```

## Troubleshooting

### Deployment Stuck

If deployment appears stuck:

```bash
# Check container status
docker ps | grep <app-name>

# Check logs
docker logs <app-name>-green

# Force stop green container
docker stop <app-name>-green
docker rm <app-name>-green
```

### Health Check Failures

If health checks are failing:

```bash
# Test health endpoint manually
curl http://localhost:<port>/health

# Check application logs
docker logs <app-name>-green

# Verify environment variables
docker exec <app-name>-green env
```

### Container Not Starting

If containers fail to start:

```bash
# Check container logs
docker logs <app-name>-green

# Check image
docker images | grep <app-name>

# Rebuild image
docker build -t <app-name>:latest .
```

## Best Practices

### 1. Health Check Endpoint

Always implement a health check endpoint:

```go
// Go example
func healthHandler(w http.ResponseWriter, r *http.Request) {
    w.WriteHeader(http.StatusOK)
    w.Write([]byte("healthy"))
}
```

```python
# Python example
@app.route('/health')
def health():
    return {"status": "healthy"}, 200
```

### 2. Graceful Shutdown

Implement graceful shutdown handling:

```go
// Go example
func main() {
    server := &http.Server{Addr: ":8080"}
    
    // Handle shutdown signals
    c := make(chan os.Signal, 1)
    signal.Notify(c, os.Interrupt, syscall.SIGTERM)
    
    go func() {
        <-c
        server.Shutdown(context.Background())
    }()
    
    server.ListenAndServe()
}
```

### 3. Database Migrations

Run database migrations before deployment:

```bash
# Set migration command
gokku config set MIGRATION_COMMAND="python manage.py migrate" -a <app>-<env>
```

### 4. Environment Variables

Use environment variables for configuration:

```bash
# Set required environment variables
gokku config set DATABASE_URL="postgres://..." -a <app>-<env>
gokku config set REDIS_URL="redis://..." -a <app>-<env>
```

## Configuration Reference

### Environment Variables

| Variable | Description | Default |
|----------|-------------|---------|
| `ZERO_DOWNTIME` | Enable zero-downtime deployment | `false` |
| `HEALTH_CHECK_PATH` | Health check endpoint | `/health` |
| `HEALTH_CHECK_TIMEOUT` | Health check timeout (seconds) | `30` |
| `MIGRATION_COMMAND` | Command to run before deployment | - |

### Container Configuration

| Setting | Description | Default |
|---------|-------------|---------|
| `restart_policy` | Container restart policy | `always` |
| `restart_delay` | Restart delay (seconds) | `5` |
| `keep_images` | Number of images to keep | `5` |

## Examples

### Complete Configuration

```yaml
# gokku.yml
apps:
  api:
    path: ./cmd/api
    binary_name: api
    deployment:
      keep_images: 5
      restart_policy: always
      restart_delay: 5
```

```bash
# Environment configuration
gokku config set ZERO_DOWNTIME=true -a api-production
gokku config set HEALTH_CHECK_PATH=/api/health -a api-production
gokku config set HEALTH_CHECK_TIMEOUT=60 -a api-production
gokku config set DATABASE_URL="postgres://..." -a api-production
```

### Deployment Commands

```bash
# Deploy with zero-downtime
git push api-production main

# Check deployment status
gokku status -a api-production

# View logs
gokku logs -a api-production -f

# Rollback if needed
gokku rollback -a api-production
```

## Next Steps

- [Configuration Reference](/reference/configuration) - Complete config options
- [CLI Reference](/reference/cli) - Command-line tools
- [Troubleshooting](/reference/troubleshooting) - Common issues
