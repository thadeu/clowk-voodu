#!/bin/bash
set -e

echo "Building Gokku binaries for multiple platforms..."

if [ -f "bin/.gitkeep" ]; then
    mv bin/.gitkeep /tmp/.gitkeep
fi
rm -rf bin/
mkdir -p bin/
if [ -f "/tmp/.gitkeep" ]; then
    mv /tmp/.gitkeep bin/.gitkeep
fi

platforms=(
    "linux/amd64"
    "linux/arm64"
    "darwin/amd64"
    "darwin/arm64"
)

for platform in "${platforms[@]}"; do
    IFS='/' read -r os arch <<< "$platform"
    echo "Building for $os/$arch..."

    binary_name="gokku-$os-$arch"

    # Build with optimizations to reduce binary size
    GOMAXPROCS=1 GOOS=$os GOARCH=$arch go build -ldflags="-s -w" -o "bin/$binary_name" ./cmd/cli

    echo "✓ Built bin/$binary_name"
done

echo ""
echo "Creating .tar.gz archives..."

for platform in "${platforms[@]}"; do
    IFS='/' read -r os arch <<< "$platform"
    binary_name="gokku-$os-$arch"
    archive_name="gokku-$os-$arch.tar.gz"

    echo "Creating $archive_name..."
    tar -czf "bin/$archive_name" -C bin "$binary_name"
    rm -rf bin/$binary_name
    echo "✓ Created bin/$archive_name"
done

echo ""
echo "Archives created in bin/ directory:"

ls -la bin/*.tar.gz

echo ""
echo "To create checksums:"
echo "  cd bin/ && sha256sum *.tar.gz > SHA256SUMS"
