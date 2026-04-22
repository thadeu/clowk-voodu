# Multi-App Project Example

Deploy multiple applications from a single repository (monorepo).

## Basic Multi-App Setup

### Project Structure

```
my-project/
├── cmd/
│   ├── api/
│   │   └── main.go
│   ├── worker/
│   │   └── main.go
│   └── admin/
│       └── main.go
├── services/
│   └── ml/
│       ├── server.py
│       └── requirements.txt
├── go.mod
├── go.sum
└── gokku.yml
```

### gokku.yml

```yaml
apps:
  api:
    path: ./cmd/api
    binary_name: api
  
  worker:
    path: ./cmd/worker
    binary_name: worker
  
  admin:
    path: ./cmd/admin
    binary_name: admin
  
  ml-service:
    lang: python
    path: ./services/ml
    entrypoint: server.py
```

### Setup All Apps

```bash
# On local machine
gokku remote add api-production ubuntu@server
gokku remote add worker-production ubuntu@server
gokku remote add admin-production ubuntu@server
gokku remote add ml-service-production ubuntu@server
```

### Deploy

```bash
# Deploy all to production
git push api-production main
git push worker-production main
git push admin-production main
git push ml-service-production main
```

## Microservices Architecture

### API + Worker + Admin

```yaml
apps:
  api:
    path: ./cmd/api
  
  worker:
    path: ./cmd/worker
  
  admin:
    path: ./cmd/admin
```

### Service Communication

```go
// cmd/api/main.go
package main

import (
    "net/http"
    "os"
)

func main() {
    workerURL := os.Getenv("WORKER_URL")
    
    http.HandleFunc("/process", func(w http.ResponseWriter, r *http.Request) {
        // Call worker service
        resp, _ := http.Post(workerURL+"/job", "application/json", r.Body)
        // ...
    })
    
    http.ListenAndServe(":"+os.Getenv("PORT"), nil)
}
```

```go
// cmd/worker/main.go
package main

import (
    "net/http"
    "os"
)

func main() {
    http.HandleFunc("/job", func(w http.ResponseWriter, r *http.Request) {
        // Process job
        w.Write([]byte("Job processed"))
    })
    
    http.ListenAndServe(":"+os.Getenv("PORT"), nil)
}
```

## Different Technologies

Mix Go, Python, Node.js in one project:

```yaml
apps:
  # Go API
  api:
    path: ./cmd/api
  
  # Python ML Service
   ml:
    lang: python
    path: ./services/ml
    entrypoint: server.py
  
  # Node.js Frontend
  frontend:
    lang: nodejs
    path: ./frontend
    entrypoint: server.js
```

## Shared Dependencies

### Go Modules (Shared)

```
my-project/
├── cmd/
│   ├── api/
│   └── worker/
├── internal/
│   ├── models/
│   └── db/
├── go.mod
└── gokku.yml
```

Both API and Worker share `internal/` packages.

### Python (Shared)

```
my-project/
├── services/
│   ├── ml/
│   │   └── server.py
│   └── worker/
│       └── worker.py
├── shared/
│   └── utils.py
└── gokku.yml
```

```python
# services/ml/server.py
import sys
sys.path.append('../../')
from shared import utils
```

## Different Versions

Each app can use different tool versions:

```yaml
apps:
  api-v1:
    path: ./cmd/api-v1
      go_version: "1.24"
      
  api-v2:
    path: ./cmd/api-v2
    go_version: "1.25"
```

Or with `.tool-versions`:

```
cmd/api-v1/.tool-versions:
golang 1.24.0

cmd/api-v2/.tool-versions:
golang 1.25.0
```

## Background Workers

### Cron-like Worker

```go
// cmd/cron-worker/main.go
package main

import (
    "log"
    "time"
)

func main() {
    ticker := time.NewTicker(1 * time.Hour)
    defer ticker.Stop()
    
    for range ticker.C {
        log.Println("Running scheduled task...")
        // Do work
    }
}
```

```yaml
apps:
  cron-worker:
    path: ./cmd/cron-worker
```

### Queue Worker (Celery)

```python
# services/worker/worker.py
from celery import Celery

app = Celery('tasks', broker='redis://localhost:6379')

@app.task
def process_video(video_id):
    # Process
    return f"Done: {video_id}"

if __name__ == '__main__':
    app.worker_main()
```

```yaml
apps:
  celery-worker:
    lang: python
    path: ./services/worker
    entrypoint: worker.py
```

## Staging + Production

Each app with both environments:

```yaml
apps:
  api:
    path: ./cmd/api
  
  worker:
    path: ./cmd/worker
```

### Deploy

```bash
# Production
gokku remote add api-production ubuntu@server
gokku remote add worker-production ubuntu@server

# Staging
gokku remote add api-staging ubuntu@server
gokku remote add worker-staging ubuntu@server

# Deploy to staging
git push api-staging staging
git push worker-staging staging

# Deploy to production
git push api-prod main
git push worker-prod main
```

## Database Migrations

### Separate Migration App

```yaml
apps:
  api:
    path: ./cmd/api
  
  migrate:
    path: ./cmd/migrate
```

```go
// cmd/migrate/main.go
package main

import (
    "database/sql"
    "log"
    "os"
    
    _ "github.com/lib/pq"
)

func main() {
    db, _ := sql.Open("postgres", os.Getenv("DATABASE_URL"))
    
    // Run migrations
    _, err := db.Exec(`
        CREATE TABLE IF NOT EXISTS users (
            id SERIAL PRIMARY KEY,
            name VARCHAR(100)
        )
    `)
    
    if err != nil {
        log.Fatal(err)
    }
    
    log.Println("Migrations complete")
}
```

Deploy migrations before API:

```bash
git push migrate-prod main
git push api-prod main
```

## Load Balancer Setup

With multiple instances:

```yaml
apps:
  api-1:
    path: ./cmd/api
  
  api-2:
    path: ./cmd/api
```

Use nginx for load balancing:

```nginx
upstream api_backend {
    server localhost:8080;
    server localhost:8081;
}

server {
    listen 80;
    
    location / {
        proxy_pass http://api_backend;
    }
}
```

## Monitoring All Apps

### Health Check Aggregator

```go
// cmd/health-checker/main.go
package main

import (
    "net/http"
    "encoding/json"
)

func main() {
    http.HandleFunc("/health/all", func(w http.ResponseWriter, r *http.Request) {
        services := map[string]string{
            "api":    checkHealth("http://localhost:8080/health"),
            "worker": checkHealth("http://localhost:8081/health"),
            "ml":     checkHealth("http://localhost:8082/health"),
        }
        json.NewEncoder(w).Encode(services)
    })
    
    http.ListenAndServe(":9000", nil)
}

func checkHealth(url string) string {
    resp, err := http.Get(url)
    if err != nil || resp.StatusCode != 200 {
        return "unhealthy"
    }
    return "healthy"
}
```

## Complete Example

Full monorepo example: [github.com/thadeu/gokku-examples/monorepo](https://github.com/thadeu/gokku-examples/tree/main/monorepo)

## Best Practices

1. **Shared Code**: Keep shared code in `internal/` or `pkg/`
2. **Independent Deploys**: Each app deploys independently
3. **Environment Parity**: Keep staging and production configs similar
4. **Service Discovery**: Use environment variables for service URLs
5. **Database Migrations**: Deploy migrations before apps
6. **Health Checks**: Each service should have `/health` endpoint

## Next Steps

- [Configuration](/guide/configuration) - Advanced multi-app config
- [Environments](/guide/environments) - Manage multiple environments
- [Docker Support](/guide/docker) - Advanced Docker configuration

