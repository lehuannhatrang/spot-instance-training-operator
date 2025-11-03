#!/bin/bash
set -e

VERSION="${1:-v1.0.1}"
IMAGE="docker.io/lehuannhatrang/spot-instance-training-operator:${VERSION}"

echo "Building version ${VERSION}..."

# Build
make build

# Build and push image
make docker-build docker-push IMG="${IMAGE}"

# Update deployment
kubectl set image deployment/spot-training-operator-controller-manager \
  -n spot-training-operator-system \
  manager="${IMAGE}" || echo "Not yet deployed, run: make deploy IMG=${IMAGE}"

echo "✅ Done! Image: ${IMAGE}"

