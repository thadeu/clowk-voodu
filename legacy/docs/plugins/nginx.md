# Nginx Plugin

The Nginx plugin provides a powerful load balancer and reverse proxy service with SSL termination, health checks, and domain management capabilities.

## Installation

```bash
gokku plugins:add nginx
```

## Features

- **Load Balancing**: Distribute traffic across multiple backend services
- **Reverse Proxy**: Route requests to different applications
- **SSL Termination**: Handle SSL certificates and HTTPS
- **Static File Serving**: Serve static assets efficiently
- **Health Checks**: Monitor backend service health
- **Configuration Management**: Easy nginx configuration updates
- **Domain Management**: Automatic domain and upstream configuration

## Usage

### Create Nginx Service

```bash
# Create with default configuration
gokku services:create nginx --name nginx-lb

# Create with specific version
gokku services:create nginx:1.25 --name nginx-staging
gokku services:create nginx:alpine --name nginx-production
```

Available versions: Any Nginx Docker tag (1.25, 1.24, latest, alpine variants, etc.)

### Link Service to App

```bash
gokku services:link nginx-lb -a api-production
```

### Domain Management

```bash
# Add domain for an app
gokku nginx:add-domain nginx-lb api api.example.com

# Add multiple domains
gokku nginx:add-domain nginx-lb api api.example.com
gokku nginx:add-domain nginx-lb api api-staging.example.com

# List all configured domains
gokku nginx:list-domains nginx-lb

# Get domain for specific app
gokku nginx:get-domain nginx-lb api

# Remove domain for an app
gokku nginx:remove-domain nginx-lb api
```

### Upstream Management

```bash
# Add upstream for an app
gokku nginx:add-upstream nginx-lb api

# Update upstream configuration
gokku nginx:update-upstream nginx-lb api

# Remove upstream for an app
gokku nginx:remove-upstream nginx-lb api
```

### Service Management

```bash
# Show service information
gokku nginx:info nginx-lb

# View service logs
gokku nginx:logs nginx-lb

# Reload nginx configuration
gokku nginx:reload nginx-lb

# Check nginx status
gokku nginx:status nginx-lb

# Test nginx configuration
gokku nginx:test nginx-lb

# Show nginx configuration
gokku nginx:config nginx-lb
```

## Configuration

### Environment Variables

Set nginx-specific environment variables:

```bash
# Set nginx worker processes
gokku config set NGINX_WORKER_PROCESSES=auto -a nginx-lb

# Set worker connections
gokku config set NGINX_WORKER_CONNECTIONS=1024 -a nginx-lb

# Set keepalive timeout
gokku config set NGINX_KEEPALIVE_TIMEOUT=65 -a nginx-lb

# Set client max body size
gokku config set NGINX_CLIENT_MAX_BODY_SIZE=10m -a nginx-lb
```

### Custom Configuration

The nginx service creates a configuration directory at `/opt/gokku/services/<service-name>/` with:

- `nginx.conf` - Main nginx configuration
- `conf.d/` - Directory for site-specific configurations
- `ssl/` - SSL certificates directory

### Example Configuration

```nginx
# /opt/gokku/services/nginx-lb/conf.d/api.conf
upstream api_backend {
    server api-production:8080;
    server api-production-2:8080;
}

server {
    listen 80;
    server_name api.example.com;
    
    location / {
        proxy_pass http://api_backend;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}
```

## Load Balancing Methods

### Round Robin (Default)

```nginx
upstream backend {
    server app1:8080;
    server app2:8080;
    server app3:8080;
}
```

### Least Connections

```nginx
upstream backend {
    least_conn;
    server app1:8080;
    server app2:8080;
}
```

### IP Hash

```nginx
upstream backend {
    ip_hash;
    server app1:8080;
    server app2:8080;
}
```

### Weighted Round Robin

```nginx
upstream backend {
    server app1:8080 weight=3;
    server app2:8080 weight=1;
}
```

## Health Checks

Configure health checks for your backends:

```nginx
upstream backend {
    server app1:8080 max_fails=3 fail_timeout=30s;
    server app2:8080 max_fails=3 fail_timeout=30s;
    server app3:8080 backup;
}
```

## SSL Configuration

### Manual SSL Setup

```bash
# Copy SSL certificates
cp server.crt /opt/gokku/services/nginx-lb/ssl/
cp server.key /opt/gokku/services/nginx-lb/ssl/

# Reload nginx to apply SSL configuration
gokku nginx:reload nginx-lb
```

### SSL Configuration Example

```nginx
server {
    listen 80;
    server_name api.example.com;
    return 301 https://$server_name$request_uri;
}

server {
    listen 443 ssl;
    server_name api.example.com;
    
    ssl_certificate /etc/nginx/ssl/server.crt;
    ssl_certificate_key /etc/nginx/ssl/server.key;
    
    location / {
        proxy_pass http://api_backend;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}
```

## Static File Serving

### Basic Static Files

```nginx
server {
    listen 80;
    server_name static.example.com;
    root /var/www/static;
    
    location / {
        try_files $uri $uri/ =404;
    }
    
    location ~* \.(js|css|png|jpg|jpeg|gif|ico|svg)$ {
        expires 1y;
        add_header Cache-Control "public, immutable";
    }
}
```

### Advanced Static Configuration

```nginx
server {
    listen 80;
    server_name static.example.com;
    root /var/www/static;
    
    # Gzip compression
    gzip on;
    gzip_vary on;
    gzip_min_length 1024;
    gzip_types text/plain text/css application/json application/javascript text/xml application/xml application/xml+rss text/javascript;
    
    # Security headers
    add_header X-Frame-Options "SAMEORIGIN" always;
    add_header X-Content-Type-Options "nosniff" always;
    add_header X-XSS-Protection "1; mode=block" always;
    
    location / {
        try_files $uri $uri/ =404;
    }
}
```

## Monitoring

### Access Logs

```bash
# View access logs
gokku nginx:logs nginx-lb

# View error logs
docker exec nginx-lb tail -f /var/log/nginx/error.log

# View access logs with real-time monitoring
docker exec nginx-lb tail -f /var/log/nginx/access.log
```

### Performance Monitoring

```bash
# Check nginx status
gokku nginx:status nginx-lb

# Test configuration
gokku nginx:test nginx-lb

# Show current configuration
gokku nginx:config nginx-lb
```

## Common Use Cases

### API Gateway

```nginx
# Route different APIs
server {
    listen 80;
    server_name api.example.com;
    
    location /v1/ {
        proxy_pass http://api-v1-backend;
    }
    
    location /v2/ {
        proxy_pass http://api-v2-backend;
    }
    
    location /admin/ {
        proxy_pass http://admin-backend;
    }
}
```

### Microservices Load Balancer

```nginx
# User service
upstream user-service {
    server user-service-1:8080;
    server user-service-2:8080;
}

# Order service
upstream order-service {
    server order-service-1:8080;
    server order-service-2:8080;
}

server {
    listen 80;
    server_name api.example.com;
    
    location /users/ {
        proxy_pass http://user-service;
    }
    
    location /orders/ {
        proxy_pass http://order-service;
    }
}
```

### WebSocket Support

```nginx
server {
    listen 80;
    server_name ws.example.com;
    
    location / {
        proxy_pass http://websocket-backend;
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection "upgrade";
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}
```

## Troubleshooting

### Configuration Issues

```bash
# Test nginx configuration
gokku nginx:test nginx-lb

# Show current configuration
gokku nginx:config nginx-lb

# Check for syntax errors
docker exec nginx-lb nginx -t
```

### Common Issues

1. **Port conflicts**: The plugin automatically assigns available ports
2. **Configuration errors**: Use `nginx:test` to validate configuration
3. **Backend connectivity**: Check if backend services are running
4. **SSL issues**: Verify certificate paths and permissions

### Debug Commands

```bash
# Check nginx processes
docker exec nginx-lb ps aux

# Check nginx version
docker exec nginx-lb nginx -v

# Check loaded modules
docker exec nginx-lb nginx -V

# Check configuration syntax
docker exec nginx-lb nginx -t
```

## Best Practices

1. **Health Checks**: Configure proper health checks for backends
2. **SSL/TLS**: Always use HTTPS in production
3. **Security Headers**: Add security headers for protection
4. **Compression**: Enable gzip compression for better performance
5. **Monitoring**: Regularly monitor access and error logs
6. **Backup**: Keep configuration backups

## Examples

### Complete Setup

```bash
# 1. Install the plugin
gokku plugins:add nginx

# 2. Create nginx service
gokku services:create nginx --name nginx-lb

# 3. Add domains for apps
gokku nginx:add-domain nginx-lb api api.example.com
gokku nginx:add-domain nginx-lb web www.example.com

# 4. Add upstreams for apps
gokku nginx:add-upstream nginx-lb api
gokku nginx:add-upstream nginx-lb web

# 5. Check configuration
gokku nginx:info nginx-lb
gokku nginx:list-domains nginx-lb

# 6. Test configuration
gokku nginx:test nginx-lb
```

### Multiple Environments

```bash
# Production load balancer
gokku services:create nginx --name nginx-prod
gokku nginx:add-domain nginx-prod api api.example.com

# Staging load balancer
gokku services:create nginx --name nginx-staging
gokku nginx:add-domain nginx-staging api api-staging.example.com

# Development load balancer
gokku services:create nginx --name nginx-dev
gokku nginx:add-domain nginx-dev api api-dev.example.com
```

## Security

- **SSL/TLS**: Always use HTTPS in production
- **Security Headers**: Add appropriate security headers
- **Access Control**: Restrict access to sensitive endpoints
- **Rate Limiting**: Implement rate limiting for API endpoints
- **Firewall**: Use proper firewall rules

## Next Steps

- [PostgreSQL Plugin](/guide/plugins-postgres) - Add relational database
- [Redis Plugin](/guide/plugins-redis) - Add caching layer
- [Let's Encrypt Plugin](/guide/plugins-letsencrypt) - Add SSL certificates
- [Environment Variables](/guide/env-vars) - Configure your app
