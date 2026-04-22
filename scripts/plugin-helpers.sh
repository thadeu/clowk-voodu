#!/bin/bash
# Voodu plugin helper library.
#
# Plugins can source this file to get a small standard library of helpers
# (env file manipulation, container inspection, service storage, logging)
# plus the VOODU_* path variables documented in pkg/plugin.
#
# Usage (inside a plugin command):
#
#     # shellcheck source=/dev/null
#     source "${VOODU_ROOT:-/opt/voodu}/scripts/plugin-helpers.sh"
#
# The controller always injects VOODU_ROOT and VOODU_PLUGIN_DIR when it
# invokes a plugin command, so sourcing is safe without extra guards.

export VOODU_BIN_DIR="${VOODU_BIN_DIR:-/usr/local/bin}"
export VOODU_ROOT="${VOODU_ROOT:-/opt/voodu}"
export VOODU_SCRIPTS_DIR="$VOODU_ROOT/scripts"
export VOODU_OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
export VOODU_ARCH="$(uname -m | tr '[:upper:]' '[:lower:]')"

# get_next_port prints the next TCP port not in LISTEN state at or above
# the given lower bound (default 3000).
get_next_port() {
    local port="${1:-3000}"

    while netstat -ln 2>/dev/null | grep -q ":$port " || ss -ln 2>/dev/null | grep -q ":$port "; do
        port=$((port + 1))
    done

    echo "$port"
}

# generate_password returns a 25-char URL-safe random string.
generate_password() {
    openssl rand -base64 32 | tr -d "=+/" | cut -c1-25
}

# set_app_env upserts KEY=VALUE into /opt/voodu/apps/<app>/<env>/.env.
set_app_env() {
    local app_name="$1" env="$2" key="$3" value="$4"

    local env_file="$VOODU_ROOT/apps/$app_name/$env/.env"

    mkdir -p "$(dirname "$env_file")"

    if grep -q "^$key=" "$env_file" 2>/dev/null; then
        local tmp
        tmp="$(mktemp)"

        while IFS= read -r line; do
            if [[ "$line" =~ ^$key= ]]; then
                echo "$key=$value"
            else
                echo "$line"
            fi
        done < "$env_file" > "$tmp"

        mv "$tmp" "$env_file"
    else
        echo "$key=$value" >> "$env_file"
    fi
}

unset_app_env() {
    local app_name="$1" env="$2" key="$3"

    local env_file="$VOODU_ROOT/apps/$app_name/$env/.env"

    if [ -f "$env_file" ]; then
        local tmp
        tmp="$(mktemp)"

        while IFS= read -r line; do
            if [[ ! "$line" =~ ^$key= ]]; then
                echo "$line"
            fi
        done < "$env_file" > "$tmp"

        mv "$tmp" "$env_file"
    fi
}

get_app_base_port() {
    local app_name="$1" env="$2"

    local env_file="$VOODU_ROOT/apps/$app_name/$env/.env"

    if [ -f "$env_file" ]; then
        grep "^PORT=" "$env_file" | cut -d= -f2 | head -1
    fi
}

container_exists() {
    docker ps -aq -f name="^$1$" | grep -q .
}

container_is_running() {
    docker ps -q -f name="^$1$" | grep -q .
}

get_container_port() {
    docker port "$1" "$2" 2>/dev/null | cut -d: -f2
}

get_container_status() {
    docker inspect --format='{{.State.Status}}' "$1" 2>/dev/null || echo "unknown"
}

get_container_started() {
    docker inspect --format='{{.State.StartedAt}}' "$1" 2>/dev/null || echo "unknown"
}

get_container_uptime() {
    local started
    started="$(get_container_started "$1")"

    if [ "$started" = "unknown" ]; then
        echo "unknown"
        return
    fi

    local start_time current_time uptime
    start_time="$(date -d "$started" +%s 2>/dev/null || echo 0)"
    current_time="$(date +%s)"
    uptime=$((current_time - start_time))

    if [ "$uptime" -le 0 ]; then
        echo "unknown"
        return
    fi

    local days=$((uptime / 86400))
    local hours=$(((uptime % 86400) / 3600))
    local minutes=$(((uptime % 3600) / 60))

    if [ "$days" -gt 0 ]; then
        echo "${days}d ${hours}h ${minutes}m"
    elif [ "$hours" -gt 0 ]; then
        echo "${hours}h ${minutes}m"
    else
        echo "${minutes}m"
    fi
}

create_service_dir() {
    local dir="$VOODU_ROOT/services/$1"
    mkdir -p "$dir"
    echo "$dir"
}

get_service_config() {
    local cfg="$VOODU_ROOT/services/$1/config.json"

    if [ -f "$cfg" ]; then
        cat "$cfg"
    else
        echo "{}"
    fi
}

update_service_config() {
    local service_name="$1" key="$2" value="$3"

    local cfg="$VOODU_ROOT/services/$service_name/config.json"

    mkdir -p "$(dirname "$cfg")"

    if [ -f "$cfg" ]; then
        jq --arg k "$key" --arg v "$value" '.[$k] = $v' "$cfg" > "${cfg}.tmp" && mv "${cfg}.tmp" "$cfg"
    else
        jq -n --arg k "$key" --arg v "$value" '{($k): $v}' > "$cfg"
    fi
}

log_message() {
    local level="$1" message="$2"
    local ts
    ts="$(date '+%Y-%m-%d %H:%M:%S')"
    echo "[$ts] [$level] $message"
}

log_error()   { log_message "ERROR"   "$1" >&2; }
log_info()    { log_message "INFO"    "$1";      }
log_warning() { log_message "WARNING" "$1" >&2; }

command_exists() {
    command -v "$1" >/dev/null 2>&1
}

port_available() {
    ! (netstat -ln 2>/dev/null | grep -q ":$1 " || ss -ln 2>/dev/null | grep -q ":$1 ")
}

wait_for_container() {
    local name="$1" max_attempts="${2:-30}" attempt=0

    while [ "$attempt" -lt "$max_attempts" ]; do
        if container_is_running "$name"; then
            return 0
        fi

        sleep 1
        attempt=$((attempt + 1))
    done

    return 1
}

cleanup_old_containers() {
    local pattern="$1" keep="${2:-5}"

    local containers
    containers="$(docker ps -aq -f name="$pattern" --format "table {{.ID}}\t{{.CreatedAt}}" | sort -k2 | head -n -"$keep")"

    if [ -n "$containers" ]; then
        echo "$containers" | while read -r id; do
            if [ -n "$id" ]; then
                echo "Cleaning up old container: $id"
                docker rm -f "$id" 2>/dev/null || true
            fi
        done
    fi
}

export_service_data() {
    local name="$1" dest="$2"
    local dir="$VOODU_ROOT/services/$name"

    if [ -d "$dir" ]; then
        tar -czf "$dest" -C "$dir" .
        echo "Service data exported to: $dest"
    else
        echo "Service directory not found: $dir"
        return 1
    fi
}

import_service_data() {
    local name="$1" src="$2"
    local dir="$VOODU_ROOT/services/$name"

    mkdir -p "$dir"

    if [ -f "$src" ]; then
        tar -xzf "$src" -C "$dir"
        echo "Service data imported from: $src"
    else
        echo "Import file not found: $src"
        return 1
    fi
}
