# Let's Encrypt Plugin

The Let's Encrypt plugin provides automatic SSL certificate management with auto-renewal, wildcard certificate support, and seamless nginx integration.

## Installation

```bash
gokku plugins:add letsencrypt
```

## Features

- **Automatic SSL**: Generate and install SSL certificates automatically
- **Auto-Renewal**: Certificates are automatically renewed before expiration
- **Wildcard Support**: Support for wildcard certificates
- **Nginx Integration**: Seamless integration with nginx plugin
- **Multiple Domains**: Support for multiple domains and subdomains
- **Staging Environment**: Test certificates in staging environment

## Usage

### Create SSL Certificate

```bash
# Create certificate for single domain
gokku letsencrypt:create api.example.com

# Create certificate for multiple domains
gokku letsencrypt:create api.example.com,www.example.com

# Create wildcard certificate
gokku letsencrypt:create "*.example.com"
```

### Link to Nginx

```bash
# Link certificate to nginx service
gokku letsencrypt:link-nginx api.example.com nginx-lb

# Link wildcard certificate
gokku letsencrypt:link-nginx "*.example.com" nginx-lb
```

### Certificate Management

```bash
# List all certificates
gokku letsencrypt:list

# Show certificate information
gokku letsencrypt:info api.example.com

# Renew certificate manually
gokku letsencrypt:renew api.example.com

# Check certificate status
gokku letsencrypt:status api.example.com
```

### Auto-Renewal Setup

```bash
# Enable auto-renewal
gokku letsencrypt:auto-renew api.example.com

# Set up cron job for auto-renewal
gokku letsencrypt:cron

# Remove auto-renewal
gokku letsencrypt:remove-auto-renew api.example.com

# Remove cron job
gokku letsencrypt:remove-cron
```

### Service Management

```bash
# View service logs
gokku letsencrypt:logs

# Restart service
gokku letsencrypt:restart

# Unlink from nginx
gokku letsencrypt:unlink-nginx api.example.com nginx-lb
```

## Configuration

### Environment Variables

Set Let's Encrypt-specific environment variables:

```bash
# Set email for Let's Encrypt registration
gokku config set LETSENCRYPT_EMAIL=admin@example.com -a letsencrypt

# Use staging environment (for testing)
gokku config set LETSENCRYPT_STAGING=true -a letsencrypt

# Set certificate directory
gokku config set LETSENCRYPT_CERT_DIR=/opt/gokku/ssl -a letsencrypt

# Set key size
gokku config set LETSENCRYPT_KEY_SIZE=2048 -a letsencrypt
```

### Staging Environment

Use staging environment for testing:

```bash
# Enable staging mode
gokku config set LETSENCRYPT_STAGING=true -a letsencrypt

# Create test certificate
gokku letsencrypt:create api-staging.example.com

# Test certificate (staging)
gokku letsencrypt:status api-staging.example.com
```

## Certificate Types

### Single Domain Certificate

```bash
# Create certificate for single domain
gokku letsencrypt:create api.example.com
```

### Multi-Domain Certificate

```bash
# Create certificate for multiple domains
gokku letsencrypt:create api.example.com,www.example.com,admin.example.com
```

### Wildcard Certificate

```bash
# Create wildcard certificate
gokku letsencrypt:create "*.example.com"
```

## Nginx Integration

### Basic SSL Setup

```bash
# 1. Create nginx service
gokku services:create nginx --name nginx-lb

# 2. Add domain to nginx
gokku nginx:add-domain nginx-lb api api.example.com

# 3. Create SSL certificate
gokku letsencrypt:create api.example.com

# 4. Link certificate to nginx
gokku letsencrypt:link-nginx api.example.com nginx-lb

# 5. Reload nginx
gokku nginx:reload nginx-lb
```

### Advanced SSL Configuration

```nginx
# HTTP to HTTPS redirect
server {
    listen 80;
    server_name api.example.com;
    return 301 https://$server_name$request_uri;
}

# HTTPS server
server {
    listen 443 ssl;
    server_name api.example.com;
    
    ssl_certificate /etc/nginx/ssl/api.example.com.crt;
    ssl_certificate_key /etc/nginx/ssl/api.example.com.key;
    
    # SSL configuration
    ssl_protocols TLSv1.2 TLSv1.3;
    ssl_ciphers ECDHE-RSA-AES256-GCM-SHA512:DHE-RSA-AES256-GCM-SHA512:ECDHE-RSA-AES256-GCM-SHA384:DHE-RSA-AES256-GCM-SHA384;
    ssl_prefer_server_ciphers off;
    ssl_session_cache shared:SSL:10m;
    ssl_session_timeout 10m;
    
    location / {
        proxy_pass http://api_backend;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}
```

## Auto-Renewal

### Automatic Renewal Setup

```bash
# Enable auto-renewal for certificate
gokku letsencrypt:auto-renew api.example.com

# Set up cron job for auto-renewal
gokku letsencrypt:cron

# Check auto-renewal status
gokku letsencrypt:status api.example.com
```

### Manual Renewal

```bash
# Renew certificate manually
gokku letsencrypt:renew api.example.com

# Force renewal (even if not expired)
gokku letsencrypt:renew api.example.com --force
```

## Monitoring

### Certificate Status

```bash
# Check certificate status
gokku letsencrypt:status api.example.com

# List all certificates
gokku letsencrypt:list

# Show certificate information
gokku letsencrypt:info api.example.com
```

### Logs

```bash
# View service logs
gokku letsencrypt:logs

# Follow logs in real-time
gokku letsencrypt:logs -f
```

## Troubleshooting

### Certificate Creation Issues

```bash
# Check service logs
gokku letsencrypt:logs

# Test with staging environment
gokku config set LETSENCRYPT_STAGING=true -a letsencrypt
gokku letsencrypt:create api.example.com
```

### Common Issues

1. **Domain validation failed**: Ensure domain points to your server
2. **Rate limit exceeded**: Wait before creating new certificates
3. **DNS issues**: Check DNS configuration
4. **Port 80 blocked**: Ensure port 80 is accessible

### Debug Commands

```bash
# Check certificate files
ls -la /opt/gokku/ssl/

# Test certificate
openssl x509 -in /opt/gokku/ssl/api.example.com.crt -text -noout

# Check certificate expiration
openssl x509 -in /opt/gokku/ssl/api.example.com.crt -dates -noout
```

## Best Practices

1. **Use staging first**: Always test with staging environment
2. **Monitor expiration**: Set up monitoring for certificate expiration
3. **Backup certificates**: Keep backups of important certificates
4. **Use wildcards**: Use wildcard certificates for subdomains
5. **Auto-renewal**: Enable auto-renewal for production certificates

## Examples

### Complete SSL Setup

```bash
# 1. Install plugins
gokku plugins:add nginx
gokku plugins:add letsencrypt

# 2. Create nginx service
gokku services:create nginx --name nginx-lb

# 3. Add domain to nginx
gokku nginx:add-domain nginx-lb api api.example.com

# 4. Create SSL certificate
gokku letsencrypt:create api.example.com

# 5. Link certificate to nginx
gokku letsencrypt:link-nginx api.example.com nginx-lb

# 6. Enable auto-renewal
gokku letsencrypt:auto-renew api.example.com

# 7. Set up cron job
gokku letsencrypt:cron

# 8. Reload nginx
gokku nginx:reload nginx-lb
```

### Multiple Domains

```bash
# Create certificate for multiple domains
gokku letsencrypt:create api.example.com,www.example.com,admin.example.com

# Link to nginx
gokku letsencrypt:link-nginx api.example.com nginx-lb
gokku letsencrypt:link-nginx www.example.com nginx-lb
gokku letsencrypt:link-nginx admin.example.com nginx-lb
```

### Wildcard Certificate

```bash
# Create wildcard certificate
gokku letsencrypt:create "*.example.com"

# Link to nginx
gokku letsencrypt:link-nginx "*.example.com" nginx-lb
```

## Security

- **Certificate Validation**: Certificates are validated through ACME protocol
- **Private Key Security**: Private keys are stored securely
- **Auto-Renewal**: Certificates are automatically renewed before expiration
- **Staging Environment**: Test certificates in staging before production

## Next Steps

- [Nginx Plugin](/guide/plugins-nginx) - Add load balancer
- [PostgreSQL Plugin](/guide/plugins-postgres) - Add relational database
- [Redis Plugin](/guide/plugins-redis) - Add caching layer
- [Environment Variables](/guide/env-vars) - Configure your app
