# PostgreSQL Plugin

The PostgreSQL plugin provides a fully-featured PostgreSQL database service with persistent storage, automatic backups, and connection management.

## Installation

```bash
gokku plugins:add postgres
```

## Features

- **Persistent Storage**: Data survives container restarts and updates
- **Automatic Backups**: Built-in backup and restore functionality
- **Connection Management**: Automatic environment variable setup
- **Multiple Versions**: Support for PostgreSQL 14, 15, 16, 17, and Alpine variants
- **Health Monitoring**: Built-in health checks and status monitoring

## Usage

### Create PostgreSQL Service

```bash
# Create with default version (latest)
gokku services:create postgres --name db-primary

# Create with specific version
gokku services:create postgres:15 --name db-staging
gokku services:create postgres:16 --name db-production
gokku services:create postgres:15-alpine --name db-test
```

Available versions: Any PostgreSQL Docker tag (14, 15, 16, 17, latest, alpine variants, etc.)

### Link Service to App

```bash
gokku services:link db-primary -a api-production
```

This automatically adds the following environment variables to your app:

- `DATABASE_URL` - Full PostgreSQL connection string
- `POSTGRES_HOST` - PostgreSQL host (localhost)
- `POSTGRES_PORT` - PostgreSQL port
- `POSTGRES_USER` - PostgreSQL user
- `POSTGRES_PASSWORD` - PostgreSQL password
- `POSTGRES_DB` - PostgreSQL database name

### Service Information

```bash
# Show service status and information
gokku postgres:info db-primary
```

Output example:
```
PostgreSQL Service: db-primary
================================
Status: running
Port: 5432
Uptime: 2 days, 3 hours
Database: db-primary
```

### Connect to Database

```bash
# Connect to PostgreSQL CLI
gokku postgres:psql db-primary
```

Once connected, you can run SQL commands:
```sql
\l                    -- list databases
\c dbname            -- connect to database
CREATE DATABASE mydb; -- create database
\dt                  -- list tables
SELECT version();    -- check PostgreSQL version
```

### Backup and Restore

```bash
# Create backup
gokku postgres:backup db-primary > backup.sql

# Restore from backup
gokku postgres:restore db-primary backup.sql

# Backup with timestamp
gokku postgres:backup db-primary > "backup-$(date +%Y%m%d-%H%M%S).sql"
```

### View Logs

```bash
# View PostgreSQL logs
gokku postgres:logs db-primary

# Follow logs in real-time
gokku postgres:logs db-primary -f
```

### Service Management

```bash
# Unlink service from app
gokku services:unlink db-primary -a api-production

# Destroy service (WARNING: This will delete all data)
gokku services:destroy db-primary
```

## Data Persistence

Data is stored in a Docker volume named `{service-name}_data`. This ensures data persists across:

- Container restarts
- Container recreations
- System reboots
- Plugin updates

To manually inspect the volume:

```bash
# Check volume information
docker volume inspect db-primary_data

# List all volumes
docker volume ls | grep postgres
```

## Connection Examples

### Ruby/Rails

```ruby
# Using DATABASE_URL
database_url = ENV['DATABASE_URL']

# Using individual variables
host = ENV['POSTGRES_HOST']
port = ENV['POSTGRES_PORT']
database = ENV['POSTGRES_DB']
username = ENV['POSTGRES_USER']
password = ENV['POSTGRES_PASSWORD']
```

### Node.js

```javascript
// Using DATABASE_URL
const databaseUrl = process.env.DATABASE_URL;

// Using individual variables
const config = {
  host: process.env.POSTGRES_HOST,
  port: process.env.POSTGRES_PORT,
  database: process.env.POSTGRES_DB,
  user: process.env.POSTGRES_USER,
  password: process.env.POSTGRES_PASSWORD
};
```

### Python

```python
import os

# Using DATABASE_URL
database_url = os.getenv('DATABASE_URL')

# Using individual variables
config = {
    'host': os.getenv('POSTGRES_HOST'),
    'port': os.getenv('POSTGRES_PORT'),
    'database': os.getenv('POSTGRES_DB'),
    'user': os.getenv('POSTGRES_USER'),
    'password': os.getenv('POSTGRES_PASSWORD')
}
```

### Go

```go
package main

import (
    "os"
    "fmt"
)

func main() {
    // Using DATABASE_URL
    databaseURL := os.Getenv("DATABASE_URL")
    
    // Using individual variables
    host := os.Getenv("POSTGRES_HOST")
    port := os.Getenv("POSTGRES_PORT")
    database := os.Getenv("POSTGRES_DB")
    user := os.Getenv("POSTGRES_USER")
    password := os.Getenv("POSTGRES_PASSWORD")
    
    fmt.Printf("Connecting to %s:%s/%s as %s\n", host, port, database, user)
}
```

## Configuration

### Environment Variables

Set PostgreSQL-specific environment variables:

```bash
# Set PostgreSQL version
gokku config set POSTGRES_VERSION=15 -a db-primary

# Set shared buffers
gokku config set POSTGRES_SHARED_BUFFERS=256MB -a db-primary

# Set max connections
gokku config set POSTGRES_MAX_CONNECTIONS=100 -a db-primary
```

### Custom Configuration

Create custom PostgreSQL configuration:

```bash
# Create custom postgresql.conf
cat > /opt/gokku/services/db-primary/postgresql.conf << EOF
shared_buffers = 256MB
effective_cache_size = 1GB
maintenance_work_mem = 64MB
checkpoint_completion_target = 0.9
wal_buffers = 16MB
default_statistics_target = 100
EOF
```

## Monitoring

### Health Checks

```bash
# Check if service is running
gokku postgres:info db-primary

# Check PostgreSQL status
gokku postgres:psql db-primary -c "SELECT 1;"
```

### Performance Monitoring

```bash
# Connect and run monitoring queries
gokku postgres:psql db-primary
```

```sql
-- Check active connections
SELECT count(*) FROM pg_stat_activity;

-- Check database size
SELECT pg_size_pretty(pg_database_size(current_database()));

-- Check table sizes
SELECT schemaname,tablename,pg_size_pretty(size) as size
FROM (
  SELECT schemaname,tablename,pg_total_relation_size(schemaname||'.'||tablename) as size
  FROM pg_tables
  WHERE schemaname NOT IN ('information_schema','pg_catalog')
) t
ORDER BY size DESC;
```

## Troubleshooting

### Service Not Starting

```bash
# Check service logs
gokku postgres:logs db-primary

# Check container status
docker ps -a | grep db-primary

# Restart service
docker restart db-primary
```

### Connection Issues

```bash
# Check if port is accessible
telnet localhost 5432

# Check environment variables
gokku config -a api-production | grep POSTGRES
```

### Data Recovery

```bash
# List available backups
ls -la /opt/gokku/services/db-primary/backups/

# Restore from specific backup
gokku postgres:restore db-primary /path/to/backup.sql
```

## Best Practices

1. **Regular Backups**: Set up automated backups using cron
2. **Monitor Disk Space**: PostgreSQL can grow large over time
3. **Use Connection Pooling**: For high-traffic applications
4. **Tune Configuration**: Adjust settings based on your workload
5. **Test Restores**: Regularly test your backup and restore process

## Examples

### Complete Setup

```bash
# 1. Install plugin
gokku plugins:add postgres

# 2. Create database service
gokku services:create postgres:15 --name db-primary

# 3. Link to application
gokku services:link db-primary -a api-production

# 4. Verify connection
gokku postgres:psql db-primary -c "SELECT version();"

# 5. Create backup
gokku postgres:backup db-primary > initial-backup.sql
```

### Multiple Environments

```bash
# Production database
gokku services:create postgres:15 --name db-prod
gokku services:link db-prod -a api-production

# Staging database
gokku services:create postgres:15 --name db-staging
gokku services:link db-staging -a api-staging

# Development database
gokku services:create postgres:15 --name db-dev
gokku services:link db-dev -a api-development
```

## Security

- **Password Management**: Passwords are automatically generated and stored securely
- **Network Isolation**: Services run in isolated Docker networks
- **Access Control**: Only linked applications can access the database
- **Encryption**: Use SSL connections for production databases

## Next Steps

- [Redis Plugin](/guide/plugins-redis) - Add caching layer
- [Nginx Plugin](/guide/plugins-nginx) - Add load balancer
- [Environment Variables](/guide/env-vars) - Configure your app
