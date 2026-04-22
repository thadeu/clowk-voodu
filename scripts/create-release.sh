#!/bin/bash
# Create release script for Gokku
# Extracts version from main.go and creates git tag

set -e

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
NC='\033[0m'

log() {
    echo -e "${CYAN}[$(date +'%H:%M:%S')]${NC} $1"
}

log_success() {
    echo -e "${GREEN}[$(date +'%H:%M:%S')]${NC} $1"
}

log_error() {
    echo -e "${RED}[$(date +'%H:%M:%S')]${NC} $1"
}

log_warn() {
    echo -e "${YELLOW}[$(date +'%H:%M:%S')]${NC} $1"
}

# Extract version from main.go
extract_version() {
    local version_line=$(grep -E '^\s*const\s+version\s*=\s*"[^"]+"' cmd/cli/main.go)

    if [ -z "$version_line" ]; then
        log_error "Could not find version constant in cmd/cli/main.go"
        exit 1
    fi

    # Extract version string using awk (works on macOS and Linux)
    local version=$(echo "$version_line" | awk -F'"' '{print $2}')

    if [ -z "$version" ]; then
        log_error "Could not extract version from: $version_line"
        exit 1
    fi

    echo "$version"
}

# Check if tag already exists
tag_exists() {
    local tag="$1"
    git rev-parse --verify "refs/tags/$tag" >/dev/null 2>&1
}

# Validate version format (semantic versioning)
validate_version() {
    local version="$1"

    # Check if version matches semantic versioning pattern
    if [[ ! "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[a-zA-Z0-9.-]+)?(\+[a-zA-Z0-9.-]+)?$ ]]; then
        log_error "Version '$version' does not follow semantic versioning (e.g., 1.0.0, 1.0.0-beta.1)"
        exit 1
    fi

    log_success "Version format is valid: $version"
}

# Update version in docs config
update_docs_version() {
    local version="$1"
    local docs_config="docs/.vitepress/config.ts"

    if [ ! -f "$docs_config" ]; then
        log_error "Docs config file not found: $docs_config"
        exit 1
    fi

    # Check if the version is already correct
    if grep -q "text: '$version'" "$docs_config"; then
        log "Docs config already has correct version: $version"
        return 0
    fi

    # Get current version in docs config
    local current_version=$(grep -o "[0-9]\+\.[0-9]\+\.[0-9]\+" "$docs_config" | head -1)

    # Replace {{VERSION}} with actual version, or update existing version
    if sed -i.bak "s/{{VERSION}}/$version/g" "$docs_config"; then
        # Also handle case where version is already set but different
        if [ -n "$current_version" ] && [ "$current_version" != "$version" ]; then
            sed -i.bak2 "s/$current_version/$version/g" "$docs_config"
        fi
        rm -f "$docs_config.bak" "$docs_config.bak2"  # Remove backup files
        log_success "Updated version in docs config: $version"
    else
        log_error "Failed to update version in docs config"
        exit 1
    fi

    # Check if there are changes after the update
    if git diff --quiet "$docs_config"; then
        log "No changes detected in docs config after update"
        return 0
    fi

    # Commit the docs version update
    git add "$docs_config"
    if git commit -m "docs: update version to $version"; then
        log_success "Committed docs version update"
    else
        log_error "Failed to commit docs version update"
        exit 1
    fi
}

# Main function
main() {
    local auto_yes=false

    # Parse arguments
    while [[ $# -gt 0 ]]; do
        case $1 in
            -y|--yes)
                auto_yes=true
                shift
                ;;
            *)
                log_error "Unknown option: $1"
                show_help
                exit 1
                ;;
        esac
    done

    log "Creating release for Gokku..."

    # Check if we're in a git repository
    if ! git rev-parse --git-dir > /dev/null 2>&1; then
        log_error "Not in a git repository"
        exit 1
    fi

    # Check if working directory is clean
    if ! git diff-index --quiet HEAD --; then
        log_error "Working directory is not clean. Please commit or stash changes first."
        git status --porcelain
        exit 1
    fi

    # Extract version from main.go
    local version=$(extract_version)
    log "Extracted version from main.go: $version"

    # Validate version format
    validate_version "$version"

    # Update version in docs config
    update_docs_version "$version"

    # Create tag name
    local tag="v$version"

    # Check if tag already exists
    if tag_exists "$tag"; then
        log_error "Tag '$tag' already exists"
        log "Existing tag points to: $(git rev-parse "$tag")"
        log "Current HEAD: $(git rev-parse HEAD)"

        if [ "$(git rev-parse "$tag")" = "$(git rev-parse HEAD)" ]; then
            log_warn "Tag '$tag' already points to current HEAD"
            if [ "$auto_yes" = true ]; then
                git push origin "$tag"
                log_success "Tag '$tag' pushed successfully!"
                exit 0
            else
                read -p "Do you want to push the existing tag? (y/N): " -n 1 -r
                echo
                if [[ $REPLY =~ ^[Yy]$ ]]; then
                    log "Pushing existing tag..."
                    git push origin "$tag"
                    log_success "Tag '$tag' pushed successfully!"
                    exit 0
                else
                    log "Aborted"
                    exit 1
                fi
            fi
        else
            log_error "Tag '$tag' points to a different commit. Please update version in main.go or use a different version."
            exit 1
        fi
    fi

    # Create and push tag
    log "Creating tag '$tag'..."
    git tag -a "$tag" -m "Release $tag

Version: $version
Generated from commit: $(git rev-parse HEAD)
Generated by: $(whoami)@$(hostname)
Date: $(date -u +"%Y-%m-%d %H:%M:%S UTC")

This release includes:
- Binary distributions for Linux (AMD64/ARM64) and macOS (Intel/Apple Silicon)
- Automatic GitHub Release creation via CI/CD

Installation:
curl -fsSL https://gokku-vm.com/install | bash"

    log_success "Tag '$tag' created locally"

    # Ask for confirmation before pushing (unless auto_yes is set)
    if [ "$auto_yes" = true ]; then
        log "Auto-pushing tag to origin..."
        git push origin HEAD > /dev/null 2>&1
        git push origin "$tag" > /dev/null 2>&1
        log ""
        log "Monitor progress at: https://github.com/thadeu/gokku/actions"
        log "Release will be available at: https://github.com/thadeu/gokku/releases/tag/$tag"
    else
        echo ""
        log "Tag '$tag' will be pushed to origin and trigger GitHub Release creation."

        read -p "Do you want to push the tag? (y/N): " -n 1 -r
        echo

        if [[ $REPLY =~ ^[Yy]$ ]]; then
            log "Pushing tag to origin..."
            git push origin HEAD > /dev/null 2>&1
            git push origin "$tag" > /dev/null 2>&1
            log_success "Tag '$tag' pushed successfully!"
            log ""
            log "GitHub Actions will now:"
            log "  1. Build binaries for all platforms"
            log "  2. Create GitHub Release with binaries"
            log "  3. Make them available for download"
            log ""
            log "Monitor progress at: https://github.com/thadeu/gokku/actions"
            log "Release will be available at: https://github.com/thadeu/gokku/releases/tag/$tag"
        else
            log_warn "Tag created locally but not pushed. You can push it later with:"
            log "  git push origin $tag"
        fi
    fi
}

# Show help
show_help() {
    echo "Gokku Release Script"
    echo ""
    echo "Usage: $0 [OPTIONS]"
    echo ""
    echo "This script:"
    echo "  1. Extracts version from cmd/cli/main.go"
    echo "  2. Validates version format (semantic versioning)"
    echo "  3. Updates version in docs config and commits change"
    echo "  4. Creates git tag (v<version>)"
    echo "  5. Pushes tag to trigger GitHub Release"
    echo ""
    echo "Options:"
    echo "  -h, --help    Show this help"
    echo "  -y, --yes     Create and push release immediately without confirmation"
    echo "  --dry-run     Show what would be done without creating tag"
    echo ""
    echo "Examples:"
    echo "  $0                    # Create release from current version in main.go"
    echo "  $0 -y                 # Create and push release immediately"
    echo "  $0 --dry-run          # Show what would be done"
    echo ""
    echo "Version in main.go: $(extract_version 2>/dev/null || echo 'Not found')"
}

# Parse arguments
case "${1:-}" in
    -h|--help)
        show_help
        exit 0
        ;;
    --dry-run)
        log "DRY RUN MODE - No changes will be made"
        echo ""
        log "Would extract version: $(extract_version)"
        log "Would update docs config with version: $(extract_version)"
        log "Would commit docs version update"
        log "Would create tag: v$(extract_version)"
        log "Would push to: origin"
        exit 0
        ;;
    *)
        main "$@"
        ;;
esac
