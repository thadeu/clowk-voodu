# CLI Reference

Complete command-line interface reference for Gokku.

## Installation

### Client (Local Machine)

```bash
curl -fsSL https://gokku-vm.com/install | bash -s -- --client
```

### Server

```bash
curl -fsSL https://gokku-vm.com/install | bash -s -- --server
```

## Global Options

- `-a, --app <app-name>` - Specify app name (e.g., `api-production`, `vad-staging`)
- `--version, -v` - Show version information
- `--help, -h` - Show help information

The `-a/--app` flag uses your **git remote name**:
- Must be a configured git remote
- Gokku runs `git remote get-url <name>` to extract connection info
- Environment is parsed from the remote name suffix

**How it works:**

1. You add a git remote (standard git command):
   ```bash
   gokku remote add api-production ubuntu@server:api
   gokku remote add vad-staging ubuntu@server:vad
   ```

2. Gokku parses the remote URL to extract:
   - **SSH host**: `ubuntu@server`
   - **App name**: `api` (from `/repos/api.git`)

3. You use the app name directly:
   ```bash
   gokku config set PORT=8080 -a api-production
   gokku logs -a vad-staging -f
   ```

## Commands

### Remote Management

#### `gokku remote setup <user@host> [-i|--identity <pem_file>]`

Perform one-time server setup. Installs Gokku, essential plugins, and configures SSH.

```bash
gokku remote setup ubuntu@192.168.105.3
gokku remote setup ubuntu@ec2.example.com -i ~/.ssh/my-key.pem
```

#### `gokku remote add <app_name> <user@server_ip>`

Add a new git remote.

```bash
gokku remote add api-production ubuntu@54.233.138.116
```

#### `gokku remote list`

List all configured remotes.

```bash
gokku remote list
```

#### `gokku remote remove <remote_name>`

Remove a git remote.

```bash
gokku remote remove api-production
```

### Application Management

#### `gokku apps list`

List applications on the server.

```bash
gokku apps list -a api-production
```

#### `gokku apps create <app>`

Create a new application.

```bash
gokku apps create myapp -a myapp
```

#### `gokku apps destroy <app>`

Destroy an application.

```bash
gokku apps destroy myapp -a myapp
```

### Configuration

#### `gokku config set KEY=VALUE [-a <app>]`

Set environment variables.

```bash
# Remote execution
gokku config set PORT=8080 -a api-production
gokku config set DATABASE_URL="postgres://..." -a api-production

# Local execution (on server)
gokku config set PORT=8080 --app api
```

#### `gokku config get KEY [-a <app>]`

Get environment variable value.

```bash
gokku config get PORT -a api-production
```

#### `gokku config list [-a <app>]`

List all environment variables.

```bash
gokku config list -a api-production
```

#### `gokku config unset KEY [-a <app>]`

Remove environment variable.

```bash
gokku config unset PORT -a api-production
```

### Execution

#### `gokku run <command> [-a <app>]`

Run arbitrary commands inside the application container.

```bash
# Remote execution (inside container)
gokku run bundle exec rails console -a api-production
gokku run npm install -a myapp
gokku run python manage.py shell -a django-app

# Local execution (on server, inside container)
gokku run bundle exec rails console --app api
```

**Security Note:** Commands are executed inside the application's Docker container (named after the app), not directly on the server. This provides isolation and security.

**Container Naming:** The container name matches the application name (e.g., `stt`, `api-production`).

### Logs

#### `gokku logs [-a <app>] [-f]`

View application logs.

```bash
# Remote execution
gokku logs -a api-production -f
gokku logs -a api-production

# Local execution (on server)
gokku logs api production -f
```

### Status

#### `gokku status [-a <app>]`

Check service status.

```bash
# Remote execution
gokku status -a api-production

# Local execution (on server)
gokku status
```

### Restart

#### `gokku restart [-a <app>]`

Restart services.

```bash
# Remote execution
gokku restart -a api-production

# Local execution (on server)
gokku restart api
```

### Deployment

#### `gokku deploy [-a <app>]`

Deploy applications.

```bash
# Remote execution
gokku deploy -a api-production

# Local execution (on server)
gokku deploy api
```

### Rollback

#### `gokku rollback [-a <app>] [release-id]`

Rollback to previous release.

```bash
# Remote execution
gokku rollback -a api-production
gokku rollback -a api-production 2

# Local execution (on server)
gokku rollback api production
```

## Examples

### Basic Workflow

```bash
# 1. Add remote
gokku remote add api-production ubuntu@54.233.138.116

# 2. Set environment variables
gokku config set PORT=8080 -a api-production
gokku config set DATABASE_URL="postgres://..." -a api-production

# 3. Deploy application
gokku deploy -a api-production

# 4. Check status
gokku status -a api-production

# 5. View logs
gokku logs -a api-production -f
```

### Rails Application

```bash
# Connect to Rails console (inside container)
gokku run bundle exec rails console -a api-production

# Run database migrations (inside container)
gokku run bundle exec rails db:migrate -a api-production

# Check application status
gokku status -a api-production
```

### Node.js Application

```bash
# Install dependencies (inside container)
gokku run npm install -a myapp

# Run build process (inside container)
gokku run npm run build -a myapp

# Check logs
gokku logs -a myapp -f
```

### Python/Django Application

```bash
# Connect to Django shell (inside container)
gokku run python manage.py shell -a django-app

# Run migrations (inside container)
gokku run python manage.py migrate -a django-app

# Install Python packages (inside container)
gokku run pip install requests -a django-app
```

## Environment Variables

Gokku supports environment-specific configuration:

```bash
# Set production environment variables
gokku config set NODE_ENV=production -a api-production

# Set staging environment variables
gokku config set NODE_ENV=staging -a api-staging
```

## Troubleshooting

### Common Issues

1. **Remote not found**: Make sure the git remote exists
   ```bash
   git remote -v
   ```

2. **Permission denied**: Check SSH key configuration
   ```bash
   ssh -T ubuntu@your-server
   ```

3. **App not found**: Verify the app exists on the server
   ```bash
   gokku apps list -a your-app
   ```

## Next Steps

- [Configuration Reference](/reference/configuration) - Config file docs
- [Getting Started Guide](/guide/getting-started) - Quick start tutorial
- [Examples](/examples/) - Real-world usage examples

