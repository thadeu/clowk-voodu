---
layout: home

hero:
  name: Gokku
  text: Lightweight Git-Push Deployment
  tagline: Deploy Go, Python, and Node.js applications with zero hassle. Like Dokku, but focused on simplicity.
  actions:
    - theme: brand
      text: Get Started
      link: /guide/getting-started
    - theme: alt
      text: View on GitHub
      link: https://github.com/thadeu/gokku

features:
  - icon: 🚀
    title: Git Push to Deploy
    details: Deploy your applications with a simple git push. No complex CI/CD pipelines required.
  
  - icon: 🔧
    title: Multi-Language Support
    details: Native support for Go, Python, Node.js with automatic runtime detection.
  
  - icon: 🐳
    title: Docker Native
    details: All applications run in Docker containers with automatic image management and zero-downtime deployments.
  
  - icon: ⚙️
    title: Zero Configuration
    details: Sensible defaults for everything. Start with minimal config and customize as needed.
  
  
  - icon: 🔄
    title: Easy Rollback
    details: Keep multiple releases. Rollback to any previous version instantly.
---

## Quick Start

Complete deployment in 4 steps:

### 1. Setup Server

From your local machine, run one command to setup everything:

```bash
gokku remote setup user@server_ip
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
gokku remote add api-production user@server_ip
```

### 4. Deploy

```bash
git push api-production main
```

That's it! Your app is live. 🎉

---

### Alternative: Manual Installation

If you prefer to install manually:

Install Gokku on your server:

```bash
curl -fsSL https://gokku-vm.com/install | bash -s -- --server
```

Install Gokku on your client:

```bash
curl -fsSL https://gokku-vm.com/install | bash -s -- --client
```

## Why Gokku?

**Gokku** = Go + Dokku. A lightweight deployment system that:

- ✅ Works great with Go applications (but supports Python, Node.js too)
- ✅ Simple git-push workflow like Heroku/Dokku
- ✅ No Docker dependency (but Docker is supported)
- ✅ Per-app configuration and environments
- ✅ Open source and hackable

Perfect for:
- Side projects and startups
- Internal tools
- Microservices
- Development/staging environments
- Learning deployment workflows

## What's Different?

| Feature | Heroku | Dokku | Gokku |
|---------|--------|-------|-------|
| Cost | 💰 Paid | ✅ Free | ✅ Free |
| Docker Native | ✅ Yes | ✅ Yes | ✅ Yes |
| Go-First | ❌ No | ❌ No | ✅ Yes |
| Config File | ❌ No | ❌ No | ✅ Yes |
| Zero-Downtime | ⚠️ Paid | ⚠️ Complex | ✅ Built-in |

## Documentation

- 📖 [Getting Started](/guide/getting-started)
- 🔧 [Configuration](/guide/configuration)
- 🚀 [Deployment](/guide/deployment)
- 🔌 [Plugins](/guide/plugins)
- 📚 [Reference](/reference/cli)

## Community

- 🐛 [Report Issues](https://github.com/thadeu/gokku/issues)
- 💬 [Discussions](https://github.com/thadeu/gokku/discussions)

