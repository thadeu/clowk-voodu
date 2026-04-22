# Gokku Plugin Development Guide

This guide explains how to create plugins for Gokku. Plugins extend Gokku functionality by providing additional services and commands.

## Plugin Structure

Every Gokku plugin must follow this exact structure:

```
gokku-<plugin-name>/
├── README.md
├── install
├── uninstall
├── commands/
│   ├── name
│   ├── help
│   ├── info
│   ├── logs
│   └── [custom-commands]
└── hooks/
    └── [other-hooks]
```

## Required Files

### 1. `install` (Required)
**Purpose**: Install the plugin service
**Arguments**: `$1` = service name
**Permissions**: Must be executable (755)

```bash
#!/bin/bash
SERVICE_NAME="$1"

echo "-----> Installing <plugin-name> service: $SERVICE_NAME"

# Your installation logic here
# Create containers, set up configuration, etc.

echo "-----> <plugin-name> service installed successfully"
```

### 2. `uninstall` (Required)
**Purpose**: Remove the plugin service
**Arguments**: `$1` = service name
**Permissions**: Must be executable (755)

```bash
#!/bin/bash
SERVICE_NAME="$1"

echo "-----> Uninstalling <plugin-name> service: $SERVICE_NAME"

# Your cleanup logic here
# Remove containers, clean up files, etc.

echo "-----> <plugin-name> service uninstalled successfully"
```

### 3. `commands/name` (Required)
**Purpose**: Return the command name for this plugin
**Output**: Single line with command name

```bash
#!/bin/bash
echo "nginx"
```

### 4. `commands/help` (Required)
**Purpose**: Display help information for the plugin
**Output**: Multi-line help text

```bash
#!/bin/bash
cat << EOF
Nginx plugin for Gokku

Commands:
  gokku nginx:info <service>      Show service information
  gokku nginx:logs <service>       Show service logs
  gokku nginx:reload <service>     Reload nginx configuration
  gokku nginx:status <service>      Show nginx status

Examples:
  gokku nginx:info nginx-lb
  gokku nginx:reload nginx-lb
EOF
```

### 5. `commands/info` (Required)
**Purpose**: Display service information
**Arguments**: `$1` = service name
**Output**: Service status, ports, configuration details

```bash
#!/bin/bash
SERVICE_NAME="$1"

echo "Nginx Service: $SERVICE_NAME"
echo "================================"

# Check if container exists
if ! container_exists "$SERVICE_NAME"; then
    echo "Status: NOT FOUND"
    exit 0
fi

# Check if container is running
if ! container_is_running "$SERVICE_NAME"; then
    echo "Status: STOPPED"
    exit 0
fi

# Get service information
STATUS=$(get_container_status "$SERVICE_NAME")
PORT=$(get_container_port "$SERVICE_NAME" 80)
UPTIME=$(get_container_uptime "$SERVICE_NAME")

echo "Status: $STATUS"
echo "Port: $PORT"
echo "Uptime: $UPTIME"
echo "Configuration: /opt/gokku/services/$SERVICE_NAME/nginx.conf"
```

### 6. `commands/logs` (Required)
**Purpose**: Display service logs
**Arguments**: `$1` = service name
**Output**: Service logs

```bash
#!/bin/bash
SERVICE_NAME="$1"

echo "Nginx logs for $SERVICE_NAME:"
echo "============================="

# Check if container exists
if ! container_exists "$SERVICE_NAME"; then
    echo "Service '$SERVICE_NAME' not found"
    exit 1
fi

# Show logs
docker logs --tail 100 "$SERVICE_NAME"
```

## Optional Custom Commands

You can add custom commands specific to your plugin:

### Example: `commands/reload`
```bash
#!/bin/bash
SERVICE_NAME="$1"

echo "-----> Reloading nginx configuration for $SERVICE_NAME"

# Check if container is running
if ! container_is_running "$SERVICE_NAME"; then
    echo "Service '$SERVICE_NAME' is not running"
    exit 1
fi

# Reload nginx
docker exec "$SERVICE_NAME" nginx -s reload

echo "-----> Nginx configuration reloaded"
```

### Example: `commands/status`
```bash
#!/bin/bash
SERVICE_NAME="$1"

echo "Nginx status for $SERVICE_NAME:"
echo "==============================="

# Check if container is running
if ! container_is_running "$SERVICE_NAME"; then
    echo "Service '$SERVICE_NAME' is not running"
    exit 1
fi

# Get nginx status
docker exec "$SERVICE_NAME" nginx -t
docker exec "$SERVICE_NAME" nginx -s status
```

## Helper Functions

Gokku provides helper functions in `/opt/gokku/scripts/plugin-helpers.sh`:

### Container Management
- `container_exists(container_name)` - Check if container exists
- `container_is_running(container_name)` - Check if container is running
- `get_container_port(container_name, internal_port)` - Get port mapping
- `get_container_status(container_name)` - Get container status
- `get_container_uptime(container_name)` - Get container uptime

### Port Management
- `get_next_port([start_port])` - Get next available port
- `port_available(port)` - Check if port is available

### App Integration
- `set_app_env(app_name, env, key, value)` - Set app environment variable
- `unset_app_env(app_name, env, key)` - Unset app environment variable

### Service Management
- `create_service_dir(service_name)` - Create service directory
- `get_service_config(service_name)` - Get service configuration
- `update_service_config(service_name, key, value)` - Update service config

### Utilities
- `generate_password()` - Generate random password
- `log_message(level, message)` - Log with timestamp
- `command_exists(command)` - Check if command exists
- `wait_for_container(container_name, max_attempts)` - Wait for container

## Plugin Hooks

Hooks allow plugins to react to Gokku events automatically. Hooks are optional but recommended for plugins that need to integrate with core Gokku functionality.

### Hook Execution
- Hooks are executed automatically by Gokku core
- Hooks run in the plugin's directory context
- Hook failures don't affect the main operation
- All hooks are optional - plugins work without them

## Example: Nginx Plugin

Here's a complete example for a Nginx plugin:

### `install`
```bash
#!/bin/bash
SERVICE_NAME="$1"

echo "-----> Installing Nginx service: $SERVICE_NAME"

# Get next available port
PORT=$(get_next_port 80)

# Create nginx service directory
SERVICE_DIR=$(create_service_dir "$SERVICE_NAME")
mkdir -p "$SERVICE_DIR/conf.d"

# Create nginx configuration
cat > "$SERVICE_DIR/nginx.conf" << 'EOF'
user nginx;
worker_processes auto;
error_log /var/log/nginx/error.log;
pid /var/run/nginx.pid;

events {
    worker_connections 1024;
}

http {
    include /etc/nginx/mime.types;
    default_type application/octet-stream;
    
    log_format main '$remote_addr - $remote_user [$time_local] "$request" '
                    '$status $body_bytes_sent "$http_referer" '
                    '"$http_user_agent" "$http_x_forwarded_for"';
    
    access_log /var/log/nginx/access.log main;
    
    sendfile on;
    tcp_nopush on;
    tcp_nodelay on;
    keepalive_timeout 65;
    types_hash_max_size 2048;
    
    include /etc/nginx/conf.d/*.conf;
}
EOF

# Create nginx container
docker run -d \
  --name "$SERVICE_NAME" \
  -p "$PORT:80" \
  -v "$SERVICE_DIR/nginx.conf:/etc/nginx/nginx.conf:ro" \
  -v "$SERVICE_DIR/conf.d:/etc/nginx/conf.d:ro" \
  nginx:alpine

# Wait for container to be ready
wait_for_container "$SERVICE_NAME"

# Update service configuration
update_service_config "$SERVICE_NAME" "port" "$PORT"
update_service_config "$SERVICE_NAME" "config_dir" "$SERVICE_DIR"

echo "-----> Nginx service installed on port $PORT"
echo "       Configuration: $SERVICE_DIR/nginx.conf"
```

### `commands/info`
```bash
#!/bin/bash
SERVICE_NAME="$1"

echo "Nginx Service: $SERVICE_NAME"
echo "================================"

# Check if container exists
if ! container_exists "$SERVICE_NAME"; then
    echo "Status: NOT FOUND"
    exit 0
fi

# Check if container is running
if ! container_is_running "$SERVICE_NAME"; then
    echo "Status: STOPPED"
    exit 0
fi

# Get service information
STATUS=$(get_container_status "$SERVICE_NAME")
PORT=$(get_container_port "$SERVICE_NAME" 80)
UPTIME=$(get_container_uptime "$SERVICE_NAME")
CONFIG_DIR=$(get_service_config "$SERVICE_NAME" | grep -o '"config_dir":"[^"]*"' | cut -d'"' -f4)

echo "Status: $STATUS"
echo "Port: $PORT"
echo "Uptime: $UPTIME"
echo "Configuration: $CONFIG_DIR/nginx.conf"

# Show upstreams if any
if [ -d "$CONFIG_DIR/conf.d" ]; then
    echo ""
    echo "Upstreams:"
    for conf in "$CONFIG_DIR/conf.d"/*.conf; do
        if [ -f "$conf" ]; then
            UPSTREAM_NAME=$(basename "$conf" .conf)
            echo "  $UPSTREAM_NAME"
        fi
    done
fi
```

## Plugin Development Workflow

1. **Create Repository**: Create a new GitHub repository named `gokku-<plugin-name>`
2. **Implement Structure**: Create the required files with proper permissions
3. **Test Locally**: Test your plugin scripts manually
4. **Publish**: Push to GitHub
5. **Install**: Users can install with `gokku plugins:add <owner>/gokku-<plugin-name>`

## Best Practices

1. **Use Helper Functions**: Always use the provided helper functions
2. **Error Handling**: Check for errors and provide meaningful messages
3. **Cleanup**: Always clean up resources in uninstall script
4. **Documentation**: Provide clear help text and examples
5. **Testing**: Test your plugin thoroughly before publishing

## Plugin Commands

Once installed, users can:

```bash
# Install plugin
gokku plugins:add https://github.com/thadeu/gokku-nginx

# Create service
gokku services:create nginx --name nginx-lb

# Link to app
gokku services:link nginx-lb -a api

# Use plugin commands
gokku nginx:info nginx-lb
gokku nginx:logs nginx-lb
gokku nginx:reload nginx-lb
```

## Troubleshooting

- **Scripts not executable**: Ensure all scripts have 755 permissions
- **Helper functions not found**: Source the helper script in your scripts
- **Container not found**: Use `container_exists()` before operations
- **Port conflicts**: Use `get_next_port()` for port assignment

## Examples

- **Database plugins**: PostgreSQL, MySQL, Redis
- **Load balancers**: Nginx, HAProxy, Traefik
- **Monitoring**: Prometheus, Grafana
- **Storage**: MinIO, S3-compatible services
- **Message queues**: RabbitMQ, Apache Kafka
