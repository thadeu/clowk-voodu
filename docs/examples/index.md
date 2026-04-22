# Examples

Real-world examples of Gokku deployments for different use cases.

## Quick Navigation

- [Go Application](/examples/go-app) - Deploy a Go REST API
- [Python Application](/examples/python-app) - Deploy a Python Flask app
- [Rails Application](/examples/rails-app) - Deploy Ruby on Rails
- [React Application](/examples/react-app) - Deploy React with Node.js API
- [Docker Application](/examples/docker-app) - Use Docker for deployment
- [Multi-App Project](/examples/multi-app) - Deploy multiple apps from one repo

## Example Projects

### Go REST API

Simple Go API with Docker deployment:

```yaml
apps:
  api:
    path: ./cmd/api
    binary_name: api
```

[View full example →](/examples/go-app)

### Python Flask App

Python web app with Docker:

```yaml
apps:
  flask-app:
    lang: python
    path: ./app
    entrypoint: app.py
```

[View full example →](/examples/python-app)

### Docker Deployment

Using custom Dockerfile:

```yaml
apps:
  service:
    lang: python
    path: ./services/ml
    dockerfile: ./services/ml/Dockerfile
```

[View full example →](/examples/docker-app)

### Ruby on Rails

Full-stack Rails app with Docker deployment:

```yaml
apps:
  rails-app:
    lang: ruby
    path: .
```

[View full example →](/examples/rails-app)

### React with Node.js API

React frontend with Express API backend:

```yaml
apps:
  react-app:
    lang: nodejs
    path: .
```

[View full example →](/examples/react-app)

### Monorepo with Multiple Apps

Deploy multiple services from one repository:

```yaml
apps:
  api:
    path: ./cmd/api
  
  worker:
    path: ./cmd/worker
  
  ml-service:
    lang: python
    path: ./services/ml
```

[View full example →](/examples/multi-app)

## By Use Case

### Microservices

Deploy multiple independent services:
- [Multi-App Project](/examples/multi-app)

### With Database

Connect to PostgreSQL/MySQL:
- [Go Application](/examples/go-app#with-database)

### With Cache

Use Redis for caching:
- [Go Application](/examples/go-app#with-redis)

### Background Jobs

Deploy workers and cron jobs:
- [Multi-App Project](/examples/multi-app#background-workers)

### Machine Learning

Deploy ML services with dependencies:
- [Python Application](/examples/python-app#machine-learning)

### Real-time

WebSocket and real-time apps:
- [Go Application](/examples/go-app#websockets)


## By Technology

### Languages

- **Go**: [Go Application](/examples/go-app)
- **Python**: [Python Application](/examples/python-app)
- **Ruby**: [Rails Application](/examples/rails-app)
- **Node.js**: [React Application](/examples/react-app)

### Frameworks

- **Gin/Echo**: [Go Application](/examples/go-app)
- **Flask/FastAPI**: [Python Application](/examples/python-app)
- **Rails**: [Rails Application](/examples/rails-app)
- **React + Express**: [React Application](/examples/react-app)

### Deployment

- **Docker**: All applications use Docker
- **Zero-Downtime**: [Blue-Green Deployment](/guide/blue-green-deployment)

## Community Examples

Share your examples! Submit a PR:

[github.com/thadeu/gokku/tree/main/examples](https://github.com/thadeu/gokku/tree/main/examples)

## Need Help?

- [GitHub Discussions](https://github.com/thadeu/gokku/discussions)
- [GitHub Issues](https://github.com/thadeu/gokku/issues)

