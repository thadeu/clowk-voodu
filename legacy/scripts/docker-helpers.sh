#!/bin/bash
# Docker helper functions for Gokku deployment
# Provides Docker installation and verification only
# Container management is now handled by Go code

# Get script directory
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Check if docker is installed
is_docker_installed() {
    command -v docker >/dev/null 2>&1
}

# Check if docker daemon is running
is_docker_running() {
    docker ps > /dev/null 2>&1
}

# Detect Linux distribution
detect_distro() {
    if [ -f /etc/os-release ]; then
        . /etc/os-release
        echo "$ID"
    elif [ -f /etc/lsb-release ]; then
        . /etc/lsb-release
        echo "$DISTRIB_ID" | tr '[:upper:]' '[:lower:]'
    else
        echo "unknown"
    fi
}

# Install Docker on Ubuntu/Debian
install_docker_debian() {
    echo "-----> Installing Docker on Debian/Ubuntu..."
    # Update package lists
    sudo apt-get update -qq

    # Install dependencies
    echo "-----> Installing dependencies..."
    sudo apt-get install -y --no-install-recommends \
        apt-transport-https \
        ca-certificates \
        curl \
        gnupg \
        lsb-release > /dev/null 2>&1

    # Add Docker GPG key
    echo "-----> Adding Docker GPG key..."
    sudo rm -rf /usr/share/keyrings/docker-archive-keyring.gpg || true
    curl -fsSL https://download.docker.com/linux/$(lsb_release -si | tr '[:upper:]' '[:lower:]')/gpg | \
        sudo gpg --batch --yes --dearmor -o /usr/share/keyrings/docker-archive-keyring.gpg > /dev/null 2>&1

    # Add Docker repository
    echo "-----> Adding Docker repository..."
    echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/docker-archive-keyring.gpg] \
        https://download.docker.com/linux/$(lsb_release -si | tr '[:upper:]' '[:lower:]') \
        $(lsb_release -cs) stable" | \
        sudo tee /etc/apt/sources.list.d/docker.list > /dev/null

    # Update and install Docker
    echo "-----> Installing Docker CE..."
    sudo apt-get update -qq
    sudo apt-get install -y --no-install-recommends \
        docker.io \
        docker-compose-plugin \
        docker-buildx-plugin > /dev/null 2>&1

    if [ $? -eq 0 ]; then
        echo "-----> Docker installed successfully"
        return 0
    else
        echo "ERROR: Failed to install Docker"
        return 1
    fi
}

# Install Docker on Amazon Linux / RHEL / CentOS
install_docker_rhel() {
    echo "-----> Installing Docker on RHEL/CentOS/Amazon Linux..."

    # Update package lists
    sudo yum update -y -q

    # Install Docker
    echo "-----> Installing Docker..."
    sudo yum install -y docker-ce docker-ce-cli containerd.io docker-compose-plugin docker-buildx-plugin > /dev/null 2>&1

    if [ $? -eq 0 ]; then
        echo "-----> Docker installed successfully"
        return 0
    else
        echo "ERROR: Failed to install Docker"
        return 1
    fi
}

# Install Docker on Alpine
install_docker_alpine() {
    echo "-----> Installing Docker on Alpine..."
    sudo apk add --no-cache docker docker-compose docker-buildx-plugin

    if [ $? -eq 0 ]; then
        echo "-----> Docker installed successfully"
        return 0
    else
        echo "ERROR: Failed to install Docker"
        return 1
    fi
}

# Main install function
install_docker() {
    # Check if already installed
    if is_docker_installed; then
        # Start docker if not running
        if ! is_docker_running; then
            echo "-----> Starting Docker daemon..."
            sudo systemctl start docker

            # Enable docker to start on boot
            sudo systemctl enable docker > /dev/null 2>&1
        fi

        return 0
    fi

    # Detect distribution
    DISTRO=$(detect_distro)
    echo "-----> Detected Linux distribution: $DISTRO"

    case "$DISTRO" in
        ubuntu|debian)
            install_docker_debian
            ;;
        centos|rhel|fedora|amzn)
            install_docker_rhel
            ;;
        alpine)
            install_docker_alpine
            ;;
        *)
            echo "ERROR: Unsupported Linux distribution: $DISTRO"
            echo "Please install Docker manually from https://docs.docker.com/engine/install/"
            return 1
            ;;
    esac

    if [ $? -ne 0 ]; then
        return 1
    fi

    # Start Docker service
    echo "-----> Starting Docker daemon..."
    sudo systemctl start docker || {
        echo "ERROR: Failed to start Docker daemon"
        return 1
    }

    # Enable Docker to start on boot
    echo "-----> Enabling Docker to start on boot..."
    sudo systemctl enable docker > /dev/null 2>&1

    # Add current user to docker group (optional, for easier use)
    if id -nG "$USER" | grep -qw "docker"; then
        echo "-----> User already in docker group"
    else
        echo "-----> Adding user to docker group..."
        sudo usermod -aG docker "$USER" > /dev/null 2>&1
        echo "       Note: You may need to log out and log back in for group changes to take effect"
    fi

    # Wait for Docker daemon
    echo "-----> Waiting for Docker daemon to be ready..."
    local max_attempts=10
    local attempt=0
    while [ $attempt -lt $max_attempts ]; do
        if docker ps > /dev/null 2>&1; then
            echo "-----> Docker is ready"
            return 0
        fi
        attempt=$((attempt + 1))
        sleep 1
    done

    echo "ERROR: Docker daemon failed to start"
    return 1
}

# Verify Docker installation and functionality
verify_docker() {
    echo "-----> Verifying Docker installation..."

    if ! is_docker_installed; then
        echo "ERROR: Docker is not installed"
        return 1
    fi

    DOCKER_VERSION=$(docker --version)
    echo "       Docker version: $DOCKER_VERSION"

    if ! is_docker_running; then
        echo "ERROR: Docker daemon is not running"
        return 1
    fi

    # Test Docker by running hello-world
    echo "-----> Testing Docker with hello-world image..."
    if docker run --rm hello-world > /dev/null 2>&1; then
        echo "-----> Docker is working correctly"
        return 0
    else
        echo "ERROR: Docker test failed"
        return 1
    fi
}

# Get Docker status
get_docker_status() {
    if is_docker_installed; then
        if is_docker_running; then
            echo "Docker: $(docker --version) - running"
            return 0
        else
            echo "Docker: installed but not running"
            return 1
        fi
    else
        echo "Docker: not installed"
        return 1
    fi
}
