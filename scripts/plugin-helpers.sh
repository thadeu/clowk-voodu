#!/bin/bash
# Gokku Plugin Helper Scripts
# This file provides common functions for plugins

export GOKKU_BIN_DIR=/usr/local/bin
export GOKKU_DIR=/opt/gokku
export GOKKU_SCRIPTS_DIR=$GOKKU_DIR/scripts
export GOKKU_MODE=$(cat ~/.gokkurc | grep "mode" | cut -d= -f2)
export GOKKU_OS=$(uname -s | tr '[:upper:]' '[:lower:]')
export GOKKU_ARCH=$(uname -m | tr '[:upper:]' '[:lower:]')

# Get next available port
get_next_port() {
    local port=${3000:-9000}
    while netstat -ln 2>/dev/null | grep -q ":$port " || ss -ln 2>/dev/null | grep -q ":$port "; do
        port=$((port + 1))
    done
    echo $port
}

# Generate random password
generate_password() {
    openssl rand -base64 32 | tr -d "=+/" | cut -c1-25
}

# Set app environment variable
set_app_env() {
    local app_name="$1"
    local env="$2"
    local key="$3"
    local value="$4"

    local env_file="/opt/gokku/apps/$app_name/$env/.env"

    # Create directory if it doesn't exist
    mkdir -p "$(dirname "$env_file")"

    # Check if key already exists
    if grep -q "^$key=" "$env_file" 2>/dev/null; then
        # Update existing key using a more robust approach
        local temp_file=$(mktemp)
        while IFS= read -r line; do
            if [[ "$line" =~ ^$key= ]]; then
                echo "$key=$value"
            else
                echo "$line"
            fi
        done < "$env_file" > "$temp_file"
        mv "$temp_file" "$env_file"
    else
        # Add new key
        echo "$key=$value" >> "$env_file"
    fi
}

# Unset app environment variable
unset_app_env() {
    local app_name="$1"
    local env="$2"
    local key="$3"

    local env_file="/opt/gokku/apps/$app_name/$env/.env"

    if [ -f "$env_file" ]; then
        # Remove lines starting with the key using a more robust approach
        local temp_file=$(mktemp)
        while IFS= read -r line; do
            if [[ ! "$line" =~ ^$key= ]]; then
                echo "$line"
            fi
        done < "$env_file" > "$temp_file"
        mv "$temp_file" "$env_file"
    fi
}

# Get app base port
get_app_base_port() {
    local app_name="$1"
    local env="$2"

    local env_file="/opt/gokku/apps/$app_name/$env/.env"

    if [ -f "$env_file" ]; then
        grep "^PORT=" "$env_file" | cut -d= -f2 | head -1
    fi
}

# Check if container exists
container_exists() {
    local container_name="$1"
    docker ps -aq -f name="^$container_name$" | grep -q .
}

# Check if container is running
container_is_running() {
    local container_name="$1"
    docker ps -q -f name="^$container_name$" | grep -q .
}

# Get container port mapping
get_container_port() {
    local container_name="$1"
    local internal_port="$2"

    docker port "$container_name" "$internal_port" 2>/dev/null | cut -d: -f2
}

# Get container status
get_container_status() {
    local container_name="$1"
    docker inspect --format='{{.State.Status}}' "$container_name" 2>/dev/null || echo "unknown"
}

# Get container started time
get_container_started() {
    local container_name="$1"
    docker inspect --format='{{.State.StartedAt}}' "$container_name" 2>/dev/null || echo "unknown"
}

# Get container uptime
get_container_uptime() {
    local container_name="$1"
    local started=$(get_container_started "$container_name")

    if [ "$started" != "unknown" ]; then
        # Calculate uptime (basic implementation)
        local start_time=$(date -d "$started" +%s 2>/dev/null || echo "0")
        local current_time=$(date +%s)
        local uptime=$((current_time - start_time))

        if [ $uptime -gt 0 ]; then
            # Convert to human readable format
            local days=$((uptime / 86400))
            local hours=$(((uptime % 86400) / 3600))
            local minutes=$(((uptime % 3600) / 60))

            if [ $days -gt 0 ]; then
                echo "${days}d ${hours}h ${minutes}m"
            elif [ $hours -gt 0 ]; then
                echo "${hours}h ${minutes}m"
            else
                echo "${minutes}m"
            fi
        else
            echo "unknown"
        fi
    else
        echo "unknown"
    fi
}

# Create service directory
create_service_dir() {
    local service_name="$1"
    local service_dir="/opt/gokku/services/$service_name"

    mkdir -p "$service_dir"
    echo "$service_dir"
}

# Get service config
get_service_config() {
    local service_name="$1"
    local config_file="/opt/gokku/services/$service_name/config.json"

    if [ -f "$config_file" ]; then
        cat "$config_file"
    else
        echo "{}"
    fi
}

# Update service config
update_service_config() {
    local service_name="$1"
    local key="$2"
    local value="$3"

    local config_file="/opt/gokku/services/$service_name/config.json"

    # Create directory if it doesn't exist
    mkdir -p "$(dirname "$config_file")"

    # Use jq to update JSON (reliable and handles special characters properly)
    if [ -f "$config_file" ]; then
        # Update existing config
        jq --arg key "$key" --arg value "$value" '.[$key] = $value' "$config_file" > "${config_file}.tmp" && mv "${config_file}.tmp" "$config_file"
    else
        # Create new config with the key-value pair
        jq -n --arg key "$key" --arg value "$value" '{($key): $value}' > "$config_file"
    fi
}

# Log message with timestamp
log_message() {
    local level="$1"
    local message="$2"
    local timestamp=$(date '+%Y-%m-%d %H:%M:%S')

    echo "[$timestamp] [$level] $message"
}

# Error logging
log_error() {
    log_message "ERROR" "$1" >&2
}

# Info logging
log_info() {
    log_message "INFO" "$1"
}

# Warning logging
log_warning() {
    log_message "WARNING" "$1" >&2
}

# Check if command exists
command_exists() {
    command -v "$1" >/dev/null 2>&1
}

# Check if port is available
port_available() {
    local port="$1"
    ! (netstat -ln 2>/dev/null | grep -q ":$port " || ss -ln 2>/dev/null | grep -q ":$port ")
}

# Wait for container to be ready
wait_for_container() {
    local container_name="$1"
    local max_attempts="${2:-30}"
    local attempt=0

    while [ $attempt -lt $max_attempts ]; do
        if container_is_running "$container_name"; then
            return 0
        fi
        sleep 1
        attempt=$((attempt + 1))
    done

    return 1
}

# Clean up old containers
cleanup_old_containers() {
    local pattern="$1"
    local keep="${2:-5}"

    # Get list of containers matching pattern, sorted by creation time
    local containers=$(docker ps -aq -f name="$pattern" --format "table {{.ID}}\t{{.CreatedAt}}" | sort -k2 | head -n -$keep)

    if [ -n "$containers" ]; then
        echo "$containers" | while read -r container_id; do
            if [ -n "$container_id" ]; then
                echo "Cleaning up old container: $container_id"
                docker rm -f "$container_id" 2>/dev/null || true
            fi
        done
    fi
}

# Export service data
export_service_data() {
    local service_name="$1"
    local export_path="$2"

    local service_dir="/opt/gokku/services/$service_name"

    if [ -d "$service_dir" ]; then
        tar -czf "$export_path" -C "$service_dir" .
        echo "Service data exported to: $export_path"
    else
        echo "Service directory not found: $service_dir"
        return 1
    fi
}

# Import service data
import_service_data() {
    local service_name="$1"
    local import_path="$2"

    local service_dir="/opt/gokku/services/$service_name"

    mkdir -p "$service_dir"

    if [ -f "$import_path" ]; then
        tar -xzf "$import_path" -C "$service_dir"
        echo "Service data imported from: $import_path"
    else
        echo "Import file not found: $import_path"
        return 1
    fi
}
