# Gokku Plugin System

This document describes the plugin system implementation for Gokku.

## Overview

The plugin system allows extending Gokku functionality through external plugins that can be downloaded from GitHub repositories. Plugins are written in Bash and follow a specific contract.

## Architecture

### Directory Structure
```
/opt/gokku/
├── plugins/                    # Downloaded plugins
│   ├── postgres/
│   │   ├── install
│   │   ├── uninstall
│   │   └── commands/
│   │       ├── name
│   │       ├── help
│   │       ├── info
│   │       ├── logs
│   │       ├── export
│   │       ├── connect
│   │       └── destroy
│   └── redis/
├── services/                   # Active services
│   ├── postgres-api/
│   │   ├── config.json
│   │   └── logs/
│   └── redis-cache/
└── scripts/
    └── plugin-helpers.sh       # Helper functions
```

## Plugin Contract

### Required Files
Each plugin must have the following structure:

```
plugin-name/
├── install              # Installation script
├── uninstall           # Uninstallation script
└── commands/
    ├── name            # Command name (e.g., "pg")
    ├── help            # Help text
    ├── info            # Service information
    ├── logs            # Service logs
    └── [custom-commands] # Plugin-specific commands
```

### Script Requirements

#### `install`
- **Purpose**: Install the plugin service
- **Arguments**: `$1` = service name
- **Example**:
```bash
#!/bin/bash
SERVICE_NAME="$1"
# Create containers, set up configuration
```

#### `uninstall`
- **Purpose**: Remove the plugin service
- **Arguments**: `$1` = service name
- **Example**:
```bash
#!/bin/bash
SERVICE_NAME="$1"
# Remove containers, clean up
```

#### `commands/info`
- **Purpose**: Display service information
- **Arguments**: `$1` = service name
- **Example**:
```bash
#!/bin/bash
SERVICE_NAME="$1"
echo "Service: $SERVICE_NAME"
echo "Status: running"
echo "Port: 5432"
```

#### `commands/logs`
- **Purpose**: Display service logs
- **Arguments**: `$1` = service name
- **Example**:
```bash
#!/bin/bash
SERVICE_NAME="$1"
docker logs "$SERVICE_NAME"
```

## Commands

### Plugin Management
```bash
# Add plugin from GitHub
gokku plugins:add thadeu/gokku-postgres

# List installed plugins
gokku plugins:list

# Remove plugin
gokku plugins:remove postgres
```

### Service Management
```bash
# Create service from plugin
gokku services:create postgres --name postgres-api

# List services
gokku services:list

# Link service to app
gokku services:link postgres-api -a api-production

# Unlink service from app
gokku services:unlink postgres-api -a api-production

# Show service information
gokku services:info postgres-api

# Show service logs
gokku services:logs postgres-api

# Destroy service
gokku services:destroy postgres-api
```

### Plugin Commands
```bash
# Execute plugin-specific commands
gokku postgres:export postgres-api > backup.sql
gokku postgres:connect postgres-api
gokku postgres:import postgres-api < backup.sql
gokku postgres:destroy postgres-api
```

## Helper Functions

The `scripts/plugin-helpers.sh` file provides common functions for plugins:

### Container Management
- `container_exists(container_name)` - Check if container exists
- `container_is_running(container_name)` - Check if container is running
- `get_container_port(container_name, internal_port)` - Get port mapping
- `get_container_status(container_name)` - Get container status

### Port Management
- `get_next_port([start_port])` - Get next available port
- `port_available(port)` - Check if port is available

### App Integration
- `set_app_env(app_name, env, key, value)` - Set app environment variable
- `unset_app_env(app_name, env, key)` - Unset app environment variable
- `get_app_base_port(app_name, env)` - Get app base port

### Service Management
- `create_service_dir(service_name)` - Create service directory
- `get_service_config(service_name)` - Get service configuration
- `update_service_config(service_name, key, value)` - Update service config

### Utilities
- `generate_password()` - Generate random password
- `log_message(level, message)` - Log with timestamp
- `command_exists(command)` - Check if command exists
- `wait_for_container(container_name, max_attempts)` - Wait for container

## Example Plugin: PostgreSQL

### Repository Structure
```
gokku-postgres/
├── README.md
├── install
├── uninstall
└── commands/
    ├── name
    ├── help
    ├── info
    ├── logs
    ├── export
    ├── import
    ├── connect
    └── destroy
```

### `install` Script
```bash
#!/bin/bash
SERVICE_NAME="$1"

echo "-----> Installing PostgreSQL service: $SERVICE_NAME"

# Get next available port
PORT=$(get_next_port)
PASSWORD=$(generate_password)

# Create PostgreSQL container
docker run -d \
  --name "$SERVICE_NAME" \
  -p "$PORT:5432" \
  -e POSTGRES_DB="$SERVICE_NAME" \
  -e POSTGRES_USER="user" \
  -e POSTGRES_PASSWORD="$PASSWORD" \
  postgres:15

# Wait for container to be ready
wait_for_container "$SERVICE_NAME"

echo "-----> PostgreSQL service installed on port $PORT"
```

### `commands/info` Script
```bash
#!/bin/bash
SERVICE_NAME="$1"

echo "PostgreSQL Service: $SERVICE_NAME"
echo "================================"

if ! container_exists "$SERVICE_NAME"; then
    echo "Status: NOT FOUND"
    exit 0
fi

if ! container_is_running "$SERVICE_NAME"; then
    echo "Status: STOPPED"
    exit 0
fi

STATUS=$(get_container_status "$SERVICE_NAME")
PORT=$(get_container_port "$SERVICE_NAME" 5432)
UPTIME=$(get_container_uptime "$SERVICE_NAME")

echo "Status: $STATUS"
echo "Port: $PORT"
echo "Uptime: $UPTIME"
echo "Database: $SERVICE_NAME"
```

## Implementation Details

### Plugin Manager
- Downloads plugins from GitHub repositories
- Extracts tar.gz files without .git directory
- Makes all scripts executable
- Manages plugin lifecycle

### Service Manager
- Creates service configurations
- Links services to applications
- Manages service state
- Handles service destruction

### Command Routing
- Routes plugin commands to appropriate scripts
- Handles service-specific commands
- Provides error handling and validation

## Best Practices

1. **Plugin Development**
   - Use helper functions from `plugin-helpers.sh`
   - Follow the plugin contract strictly
   - Handle errors gracefully
   - Provide meaningful output

2. **Service Management**
   - Use unique service names
   - Clean up resources on uninstall
   - Provide health checks
   - Log important events

3. **Integration**
   - Set appropriate environment variables
   - Use consistent naming conventions
   - Provide clear error messages
   - Document plugin-specific commands

## Troubleshooting

### Common Issues
1. **Plugin not found**: Check if plugin is installed with `gokku plugins:list`
2. **Service not found**: Check if service exists with `gokku services:list`
3. **Command not found**: Verify plugin has the required command script
4. **Permission denied**: Ensure scripts are executable (755)

### Debug Commands
```bash
# Check plugin structure
ls -la /opt/gokku/plugins/<plugin-name>/

# Check service configuration
cat /opt/gokku/services/<service-name>/config.json

# Check app environment
cat /opt/gokku/apps/<app-name>/<env>/.env
```

## Future Enhancements

1. **Plugin Updates**: Automatic plugin updates from GitHub
2. **Plugin Dependencies**: Handle plugin dependencies
3. **Plugin Configuration**: Plugin-specific configuration files
4. **Plugin Validation**: Validate plugin contract compliance
5. **Plugin Marketplace**: Centralized plugin repository
