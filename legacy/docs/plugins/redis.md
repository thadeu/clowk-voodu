# Redis Plugin

The Redis plugin provides a high-performance in-memory data store and cache service with clustering support, persistence options, and monitoring capabilities.

## Installation

```bash
gokku plugins:add redis
```

## Features

- **In-Memory Storage**: Ultra-fast key-value operations
- **Persistence Options**: RDB snapshots and AOF logging
- **Clustering Support**: Multi-instance Redis clusters
- **Monitoring**: Built-in performance monitoring and metrics
- **Data Structures**: Support for strings, lists, sets, hashes, and more
- **Pub/Sub**: Real-time messaging and notifications

## Usage

### Create Redis Service

```bash
# Create with default configuration
gokku services:create redis --name cache-primary

# Create with specific version
gokku services:create redis:7 --name cache-staging
gokku services:create redis:7-alpine --name cache-production
```

Available versions: Any Redis Docker tag (6, 7, latest, alpine variants, etc.)

### Link Service to App

```bash
gokku services:link cache-primary -a api-production
```

This automatically adds the following environment variables to your app:

- `REDIS_URL` - Full Redis connection string
- `REDIS_HOST` - Redis host (localhost)
- `REDIS_PORT` - Redis port
- `REDIS_PASSWORD` - Redis password (if authentication enabled)

### Service Information

```bash
# Show service status and information
gokku redis:info cache-primary
```

Output example:
```
Redis Service: cache-primary
============================
Status: running
Port: 6379
Uptime: 1 day, 5 hours
Memory: 2.1MB used, 4.0MB max
Connected clients: 3
```

### Connect to Redis

```bash
# Connect to Redis CLI
gokku redis:cli cache-primary
```

Once connected, you can run Redis commands:
```redis
# Basic operations
SET mykey "Hello World"
GET mykey
DEL mykey

# List operations
LPUSH mylist "item1"
LPUSH mylist "item2"
LRANGE mylist 0 -1

# Hash operations
HSET user:1 name "John" age 30
HGET user:1 name
HGETALL user:1

# Set operations
SADD myset "member1" "member2"
SMEMBERS myset

# Check Redis info
INFO
INFO memory
INFO stats
```

### Monitor Redis

```bash
# Monitor Redis commands in real-time
gokku redis:monitor cache-primary
```

### Data Management

```bash
# Flush current database
gokku redis:flushdb cache-primary

# Flush all databases
gokku redis:flushall cache-primary

# Check database size
gokku redis:cli cache-primary -c "DBSIZE"
```

### View Logs

```bash
# View Redis logs
gokku redis:logs cache-primary

# Follow logs in real-time
gokku redis:logs cache-primary -f
```

### Service Management

```bash
# Start service
gokku redis:start cache-primary

# Stop service
gokku redis:stop cache-primary

# Restart service
gokku redis:restart cache-primary

# Unlink service from app
gokku services:unlink cache-primary -a api-production

# Destroy service (WARNING: This will delete all data)
gokku services:destroy cache-primary
```

## Configuration

### Environment Variables

Set Redis-specific environment variables:

```bash
# Set Redis version
gokku config set REDIS_VERSION=7 -a cache-primary

# Set memory limit
gokku config set REDIS_MAXMEMORY=512mb -a cache-primary

# Set persistence mode
gokku config set REDIS_SAVE="900 1 300 10 60 10000" -a cache-primary

# Enable authentication
gokku config set REDIS_REQUIREPASS=your-password -a cache-primary
```

### Custom Configuration

Create custom Redis configuration:

```bash
# Create custom redis.conf
cat > /opt/gokku/services/cache-primary/redis.conf << EOF
# Memory settings
maxmemory 512mb
maxmemory-policy allkeys-lru

# Persistence settings
save 900 1
save 300 10
save 60 10000

# Network settings
timeout 300
tcp-keepalive 60

# Logging
loglevel notice
logfile /var/log/redis/redis.log
EOF
```

## Data Persistence

### RDB Snapshots

Redis automatically creates snapshots based on configuration:

```bash
# Check RDB files
ls -la /opt/gokku/services/cache-primary/data/

# Manual snapshot
gokku redis:cli cache-primary -c "BGSAVE"
```

### AOF (Append Only File)

Enable AOF for maximum durability:

```bash
# Enable AOF
gokku redis:cli cache-primary -c "CONFIG SET appendonly yes"

# Check AOF status
gokku redis:cli cache-primary -c "CONFIG GET appendonly"
```

## Monitoring

### Performance Metrics

```bash
# Get Redis info
gokku redis:cli cache-primary -c "INFO"

# Memory usage
gokku redis:cli cache-primary -c "INFO memory"

# Statistics
gokku redis:cli cache-primary -c "INFO stats"

# Replication info
gokku redis:cli cache-primary -c "INFO replication"
```

### Health Checks

```bash
# Check if service is running
gokku redis:info cache-primary

# Test connection
gokku redis:cli cache-primary -c "PING"
```

## Connection Examples

### Ruby

```ruby
# Using REDIS_URL
require 'redis'
redis = Redis.new(url: ENV['REDIS_URL'])

# Using individual variables
redis = Redis.new(
  host: ENV['REDIS_HOST'],
  port: ENV['REDIS_PORT'],
  password: ENV['REDIS_PASSWORD']
)

# Basic operations
redis.set('key', 'value')
redis.get('key')
```

### Node.js

```javascript
const redis = require('redis');

// Using REDIS_URL
const client = redis.createClient(process.env.REDIS_URL);

// Using individual variables
const client = redis.createClient({
  host: process.env.REDIS_HOST,
  port: process.env.REDIS_PORT,
  password: process.env.REDIS_PASSWORD
});

// Basic operations
client.set('key', 'value');
client.get('key', (err, result) => {
  console.log(result);
});
```

### Python

```python
import redis
import os

# Using REDIS_URL
r = redis.from_url(os.getenv('REDIS_URL'))

# Using individual variables
r = redis.Redis(
    host=os.getenv('REDIS_HOST'),
    port=os.getenv('REDIS_PORT'),
    password=os.getenv('REDIS_PASSWORD')
)

# Basic operations
r.set('key', 'value')
r.get('key')
```

### Go

```go
package main

import (
    "github.com/go-redis/redis/v8"
    "os"
)

func main() {
    // Using REDIS_URL
    rdb := redis.NewClient(&redis.Options{
        Addr: os.Getenv("REDIS_HOST") + ":" + os.Getenv("REDIS_PORT"),
        Password: os.Getenv("REDIS_PASSWORD"),
        DB: 0,
    })
    
    // Basic operations
    rdb.Set(ctx, "key", "value", 0)
    rdb.Get(ctx, "key")
}
```

## Use Cases

### Caching

```bash
# Cache API responses
gokku redis:cli cache-primary -c "SETEX api:user:123 3600 '{\"id\":123,\"name\":\"John\"}'"

# Cache database queries
gokku redis:cli cache-primary -c "SETEX db:query:hash123 1800 'query_result'"
```

### Session Storage

```bash
# Store user sessions
gokku redis:cli cache-primary -c "SETEX session:abc123 7200 'user_data'"

# Check session expiry
gokku redis:cli cache-primary -c "TTL session:abc123"
```

### Rate Limiting

```bash
# Implement rate limiting
gokku redis:cli cache-primary -c "INCR rate_limit:user:123"
gokku redis:cli cache-primary -c "EXPIRE rate_limit:user:123 60"
```

### Pub/Sub Messaging

```bash
# Subscribe to channel
gokku redis:cli cache-primary -c "SUBSCRIBE notifications"

# Publish message
gokku redis:cli cache-primary -c "PUBLISH notifications 'Hello World'"
```

## Troubleshooting

### Service Not Starting

```bash
# Check service logs
gokku redis:logs cache-primary

# Check container status
docker ps -a | grep cache-primary

# Restart service
docker restart cache-primary
```

### Memory Issues

```bash
# Check memory usage
gokku redis:cli cache-primary -c "INFO memory"

# Check memory policy
gokku redis:cli cache-primary -c "CONFIG GET maxmemory-policy"

# Clear memory
gokku redis:flushdb cache-primary
```

### Connection Issues

```bash
# Check if port is accessible
telnet localhost 6379

# Check environment variables
gokku config -a api-production | grep REDIS
```

## Best Practices

1. **Memory Management**: Set appropriate memory limits and eviction policies
2. **Persistence**: Configure RDB snapshots and AOF for data durability
3. **Monitoring**: Regularly check memory usage and performance metrics
4. **Security**: Use authentication and network isolation
5. **Backup**: Regular backups of important data

## Examples

### Complete Setup

```bash
# 1. Install plugin
gokku plugins:add redis

# 2. Create cache service
gokku services:create redis:7 --name cache-primary

# 3. Link to application
gokku services:link cache-primary -a api-production

# 4. Verify connection
gokku redis:cli cache-primary -c "PING"

# 5. Test basic operations
gokku redis:cli cache-primary -c "SET test 'Hello Redis'"
gokku redis:cli cache-primary -c "GET test"
```

### Multiple Environments

```bash
# Production cache
gokku services:create redis:7 --name cache-prod
gokku services:link cache-prod -a api-production

# Staging cache
gokku services:create redis:7 --name cache-staging
gokku services:link cache-staging -a api-staging

# Development cache
gokku services:create redis:7 --name cache-dev
gokku services:link cache-dev -a api-development
```

## Security

- **Authentication**: Enable password protection for production
- **Network Isolation**: Services run in isolated Docker networks
- **Access Control**: Only linked applications can access Redis
- **Encryption**: Use SSL connections for production deployments

## Next Steps

- [PostgreSQL Plugin](/guide/plugins-postgres) - Add relational database
- [Nginx Plugin](/guide/plugins-nginx) - Add load balancer
- [Environment Variables](/guide/env-vars) - Configure your app
