#!/bin/bash

# Build and Push Script for Spot Instance Training Operator
# Usage: ./build-and-push.sh [version]
# Example: ./build-and-push.sh v1.0.0

set -e

# Default version if not provided
VERSION="${1:-latest}"
IMAGE_NAME="docker.io/lehuannhatrang/spot-instance-training-operator"
IMAGE_TAG="${IMAGE_NAME}:${VERSION}"

echo "======================================"
echo "Building Spot Instance Training Operator"
echo "======================================"
echo "Image: ${IMAGE_TAG}"
echo "======================================"

# Build the operator
echo "Step 1: Building the operator binary..."
make build

# Run tests (optional, comment out if you want to skip)
echo "Step 2: Running tests..."
make test || echo "Warning: Tests failed or not implemented yet"

# Build Docker image
echo "Step 3: Building Docker image..."
make docker-build IMG="${IMAGE_TAG}"

# Login to Docker Hub (you may need to run 'docker login' first)
echo "Step 4: Pushing to Docker Hub..."
echo "Make sure you're logged in to Docker Hub (run 'docker login' if not)"

make docker-push IMG="${IMAGE_TAG}"

echo "======================================"
echo "✅ Successfully built and pushed!"
echo "======================================"
echo "Image: ${IMAGE_TAG}"
echo ""
echo "To deploy to your cluster, run:"
echo "  make deploy IMG=${IMAGE_TAG}"
echo ""
echo "Or update your kustomization to use:"
echo "  newName: ${IMAGE_NAME}"
echo "  newTag: ${VERSION}"
echo "======================================"

