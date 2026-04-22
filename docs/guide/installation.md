# Installation

This guide covers installing Gokku on your server and local machine using the universal installer.

## Universal Installer

Gokku has a single universal installer that automatically detects if you're installing on a server or client.

### How It Works

The installer automatically:
- **Detects your OS and architecture** (Linux/macOS, x86_64/ARM64)
- **Downloads pre-compiled binaries** from the repository (no Go compilation needed)
- **No dependencies required** for server installation (Go only needed for client development)

**Supported platforms:**
- Linux x86_64 (amd64)
- Linux ARM64
- macOS Intel (amd64)
- macOS Apple Silicon (arm64)

### Server Installation

SSH into your server and run:

```bash
curl -fsSL https://gokku-vm.com/install | bash -s -- --server
```

Or explicitly specify server mode:

```bash
curl -fsSL https://gokku-vm.com/install | bash -s -- --server
```

This installs:
- `gokku` binary in `/usr/local/bin`
- Core scripts in `/opt/gokku/scripts/`
- Sample `gokku.yml` config

### Client/Local Installation

On your local machine:

```bash
curl -fsSL https://gokku-vm.com/install | bash
```

Or explicitly specify client mode:

```bash
curl -fsSL https://gokku-vm.com/install | bash -s -- --client
```

This installs:
- `gokku` binary in `/usr/local/bin`
- Config directory in `~/.gokku/`
- Sample config file

### Verify Installation

Check that Gokku is installed:

```bash
gokku --version
```

On server, verify files:

```bash
ls -la /opt/gokku
```

You should see:
```
/opt/gokku/
├── apps/           # Deployed applications
├── repos/          # Git repositories
├── scripts/        # Core scripts
```

## Manual Installation

If you prefer to build from source:

```bash
# Clone repository
git clone https://github.com/thadeu/gokku.git
cd gokku/infra

# Build binary
go build -o gokku ./cmd/cli

# Install
sudo mv gokku /usr/local/bin/

# Verify
gokku --version
```

## Configuration

### Create gokku.yml

In your project root, create `gokku.yml`:

```yaml
apps:
  api:
    path: ./cmd/api
    binary_name: api
```

See [Configuration](/guide/configuration) for all options.

### Setup Your App

On the server, setup your app:

```bash
# Just push - setup happens automatically!
git push api-production main
```

The first push automatically creates:
- Git repository at `api`
- App directory at `/opt/gokku/apps/api/`
- Docker container `api`
- Environment file

### Add Git Remote

On your local machine:

```bash
gokku remote add api-production ubuntu@your-server
```

Replace:
- `api-production` with your desired remote name
- `ubuntu` with your SSH user
- `your-server` with your server IP or hostname

### Test SSH Connection

```bash
ssh ubuntu@your-server
```

If connection fails, see [SSH Setup](#ssh-setup).

## SSH Setup

### Generate SSH Key

If you don't have an SSH key:

```bash
ssh-keygen -t ed25519 -C "your-email@example.com"
```

Press Enter to accept defaults.

### Copy Key to Server

```bash
ssh-copy-id ubuntu@your-server
```

Or manually:

```bash
cat ~/.ssh/id_ed25519.pub | ssh ubuntu@your-server "mkdir -p ~/.ssh && cat >> ~/.ssh/authorized_keys"
```

### Test Connection

```bash
ssh ubuntu@your-server
```

Should connect without password prompt.

## First Deployment

Deploy your app:

```bash
git push api-production main
```

You should see:

```
-----> Deploying api to production...
-----> Extracting code...
-----> Building api...
-----> Build complete (5.2M)
-----> Deploying...
-----> Restarting api-production...
-----> Deploy successful!
```

## Verify Deployment

Check container status:

```bash
ssh ubuntu@your-server "docker ps | grep api"
```

Check logs:

```bash
ssh ubuntu@your-server "docker logs -f api"
```

## Multiple Environments

Setup staging environment:

```bash
# On local machine - just add remote and push
gokku remote add api-staging ubuntu@your-server
git push api-staging develop
```

Deploy to staging:

```bash
git push api-staging develop
```

## Uninstall

### Remove Gokku from Server

```bash
# Stop all containers
docker stop $(docker ps -a | grep gokku | awk '{print $1}')
docker rm $(docker ps -a | grep gokku | awk '{print $1}')

# Remove Gokku directory
sudo rm -rf /opt/gokku

```

### Remove CLI from Local Machine

```bash
sudo rm /usr/local/bin/gokku
```

## Troubleshooting

### Permission Denied

If you get "Permission denied (publickey)":

```bash
# Copy SSH key again
ssh-copy-id ubuntu@your-server

# Or add key to ssh-agent
ssh-add ~/.ssh/id_ed25519
```

### Build Failed

Check deployment logs:

```bash
ssh ubuntu@your-server "cat /opt/gokku/apps/api/production/deploy.log"
```

### Container Won't Start

Check Docker logs:

```bash
ssh ubuntu@your-server "docker logs api"
```

## Next Steps

- [Configuration](/guide/configuration) - Customize your deployment
- [Environments](/guide/environments) - Setup staging/production
- [Environment Variables](/guide/env-vars) - Configure your app
- [Docker Support](/guide/docker) - Advanced Docker configuration

## Getting Help

- [GitHub Issues](https://github.com/thadeu/gokku/issues)
- [Discussions](https://github.com/thadeu/gokku/discussions)
- [Troubleshooting](/reference/troubleshooting)

