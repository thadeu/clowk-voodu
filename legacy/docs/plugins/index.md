# Plugins

Gokku's plugin system allows you to extend functionality with additional services like databases, load balancers, and monitoring tools.

## Available Plugins

### Core Services

#### PostgreSQL
- **Purpose**: Relational database service
- **Features**: Persistent storage, automatic backups, connection management
- **Installation**: `gokku plugins:add postgres`
- **Usage**: `gokku services:create postgres --name pg-0`

#### Redis
- **Purpose**: In-memory data store and cache
- **Features**: Clustering, persistence, monitoring
- **Installation**: `gokku plugins:add redis`
- **Usage**: `gokku services:create redis --name redis-0`

#### Nginx
- **Purpose**: Load balancer and reverse proxy
- **Features**: SSL termination, health checks, domain management
- **Installation**: `gokku plugins:add nginx`
- **Usage**: `gokku services:create nginx --name nginx-lb`

### Additional Services

#### Let's Encrypt
- **Purpose**: Automatic SSL certificate management
- **Features**: Auto-renewal, wildcard certificates, nginx integration
- **Installation**: `gokku plugins:add letsencrypt`
- **Usage**: `gokku letsencrypt:create example.com`

#### Cron
- **Purpose**: Scheduled task management
- **Features**: Job scheduling, logging, monitoring
- **Installation**: `gokku plugins:add cron`
- **Usage**: `gokku cron:schedule "0 2 * * *" "backup-script.sh"`

## Plugin Management

### Installing Plugins

```bash
# Install from GitHub repository
gokku plugins:add https://github.com/thadeu/gokku-mysql

# Install built-in plugin
gokku plugins:add mysql
```

### Listing Plugins

```bash
# List installed plugins
gokku plugins:list

# Show plugin information
gokku plugins:info mysql
```

### Removing Plugins

```bash
# Remove plugin (will also remove all services)
gokku plugins:remove mysql
```

## Service Management

### Creating Services

```bash
# Create service from plugin
gokku services:create mysql --name db-primary

# Create with specific version
gokku services:create mysql --name db-staging
```

### Linking Services to Apps

```bash
# Link service to application
gokku services:link db-primary -a api-production

# This automatically adds environment variables:
# - DATABASE_URL
# - POSTGRES_HOST, POSTGRES_PORT, etc.
```

### Managing Services

```bash
# List all services
gokku services:list

# Show service information
gokku services:info db-primary

# View service logs
gokku services:logs db-primary

# Unlink service from app
gokku services:unlink db-primary -a api-production

# Destroy service
gokku services:destroy db-primary
```

## Plugin Commands

Each plugin provides specific commands for its functionality:

### PostgreSQL Commands

```bash
# Connect to database
gokku postgres:psql db-primary

# Backup database
gokku postgres:backup db-primary > backup.sql

# Restore database
gokku postgres:restore db-primary backup.sql
```

### Nginx Commands

```bash
# Add domain for app
gokku nginx:add-domain nginx-lb api api.example.com

# Add upstream for load balancing
gokku nginx:add-upstream nginx-lb api

# Reload configuration
gokku nginx:reload nginx-lb

# Test configuration
gokku nginx:test nginx-lb
```

### Redis Commands

```bash
# Connect to Redis CLI
gokku redis:cli redis-0

# Monitor Redis commands
gokku redis:monitor redis-0

# Flush database
gokku redis:flushdb redis-0
```

## Environment Variables

When you link a service to an app, Gokku automatically adds relevant environment variables:

### PostgreSQL Variables
- `DATABASE_URL` - Full connection string
- `POSTGRES_HOST` - Database host
- `POSTGRES_PORT` - Database port
- `POSTGRES_USER` - Database user
- `POSTGRES_PASSWORD` - Database password
- `POSTGRES_DB` - Database name

### Redis Variables
- `REDIS_URL` - Full connection string
- `REDIS_HOST` - Redis host
- `REDIS_PORT` - Redis port
- `REDIS_PASSWORD` - Redis password

## Common Use Cases

### Database Setup

```bash
# 1. Install PostgreSQL plugin
gokku plugins:add postgres

# 2. Create database service
gokku services:create postgres --name db-primary

# 3. Link to application
gokku services:link db-primary -a api-production

# 4. Your app now has DATABASE_URL available
```

### Load Balancer Setup

```bash
# 1. Install Nginx plugin
gokku plugins:add nginx

# 2. Create load balancer
gokku services:create nginx --name nginx-lb

# 3. Add domain for app
gokku nginx:add-domain nginx-lb api api.example.com

# 4. Add upstream for load balancing
gokku nginx:add-upstream nginx-lb api
```

### SSL Certificate Setup

```bash
# 1. Install Let's Encrypt plugin
gokku plugins:add letsencrypt

# 2. Create SSL certificate
gokku letsencrypt:create api.example.com

# 3. Link to nginx
gokku letsencrypt:link-nginx api.example.com nginx-lb
```

## Plugin Development

Want to create your own plugin? See the [Plugin Development Guide](/plugin-system) for detailed instructions.

### Plugin Structure

```
gokku-<plugin-name>/
├── README.md
├── install
├── uninstall
└── commands/
    ├── name
    ├── help
    ├── info
    ├── logs
    └── [custom-commands]
```

### Required Commands

- `install` - Install the plugin service
- `uninstall` - Remove the plugin service
- `commands/name` - Return the command name
- `commands/help` - Display help information
- `commands/info` - Show service information
- `commands/logs` - Show service logs

## Troubleshooting

### Plugin Not Found

```bash
# Check if plugin is installed
gokku plugins:list

# Reinstall plugin
gokku plugins:remove postgres
gokku plugins:add postgres
```

### Service Not Found

```bash
# Check if service exists
gokku services:list

# Check service status
gokku services:info <service-name>
```

### Permission Issues

```bash
# Check plugin permissions
ls -la /opt/gokku/plugins/<plugin-name>/

# Fix permissions
chmod +x /opt/gokku/plugins/<plugin-name>/*
```

## Best Practices

1. **Use descriptive service names** - `db-primary`, `cache-staging`, `lb-production`
2. **Link services to apps** - This automatically provides environment variables
3. **Monitor service health** - Use `gokku services:info` regularly
4. **Backup important data** - Use plugin-specific backup commands
5. **Test configurations** - Use test commands before applying changes

## Next Steps

- [Plugin Development Guide](/plugin-system) - Create your own plugins
- [Service Configuration](/guide/configuration) - Configure your services
- [Environment Variables](/guide/env-vars) - Manage app configuration
