#!/bin/bash

# Simple Docker Build and Push Script
# Usage: ./docker-build.sh [version]
# Example: ./docker-build.sh v1.0.0

set -e

VERSION="${1:-latest}"
IMAGE_NAME="docker.io/lehuannhatrang/spot-instance-training-operator"
IMAGE_TAG="${IMAGE_NAME}:${VERSION}"

echo "======================================"
echo "Building and Pushing Docker Image"
echo "======================================"
echo "Image: ${IMAGE_TAG}"
echo "======================================"

# Build the image
echo "Building Docker image..."
docker build -t "${IMAGE_TAG}" .

# Tag as latest if building a specific version
if [ "$VERSION" != "latest" ]; then
    echo "Tagging as latest..."
    docker tag "${IMAGE_TAG}" "${IMAGE_NAME}:latest"
fi

# Push to Docker Hub
echo "Pushing to Docker Hub..."
echo "Note: Make sure you're logged in (run 'docker login' first)"
docker push "${IMAGE_TAG}"

if [ "$VERSION" != "latest" ]; then
    echo "Pushing latest tag..."
    docker push "${IMAGE_NAME}:latest"
fi

echo "======================================"
echo "✅ Successfully pushed!"
echo "======================================"
echo "Image: ${IMAGE_TAG}"
if [ "$VERSION" != "latest" ]; then
    echo "Also tagged as: ${IMAGE_NAME}:latest"
fi
echo ""
echo "To deploy to your cluster:"
echo "  kubectl apply -f config/crd/bases/"
echo "  make deploy IMG=${IMAGE_TAG}"
echo "======================================"

