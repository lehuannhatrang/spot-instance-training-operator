# Build and Deploy Guide

## Quick Reference for Building and Pushing to Docker Hub

### Prerequisites

1. Docker installed and running
2. Docker Hub account (lehuannhatrang)
3. Logged in to Docker Hub:
   ```bash
   docker login
   ```

## Option 1: Using the Build Script (Recommended)

```bash
# Build and push with version tag
./build-and-push.sh v1.0.0

# Or build and push as latest
./build-and-push.sh latest
```

This script will:
- Build the operator binary
- Run tests
- Build Docker image using Makefile
- Push to docker.io/lehuannhatrang/spot-instance-training-operator

## Option 2: Using Direct Docker Commands

```bash
# Build and push with version tag
./docker-build.sh v1.0.0

# Or build and push as latest
./docker-build.sh latest
```

This script directly uses Docker commands and will also tag as `latest`.

## Option 3: Manual Commands

### Using Make

```bash
# Build the operator
make build

# Build and push Docker image
make docker-build docker-push IMG=docker.io/lehuannhatrang/spot-instance-training-operator:v1.0.0
```

### Using Docker Directly

```bash
# Build the image
docker build -t docker.io/lehuannhatrang/spot-instance-training-operator:v1.0.0 .

# Tag as latest (optional)
docker tag docker.io/lehuannhatrang/spot-instance-training-operator:v1.0.0 \
           docker.io/lehuannhatrang/spot-instance-training-operator:latest

# Push to Docker Hub
docker push docker.io/lehuannhatrang/spot-instance-training-operator:v1.0.0
docker push docker.io/lehuannhatrang/spot-instance-training-operator:latest
```

## Deploying to Kubernetes

### 1. Install CRDs

```bash
make install
```

Or manually:
```bash
kubectl apply -f config/crd/bases/
```

### 2. Deploy the Operator

Using a specific version:
```bash
make deploy IMG=docker.io/lehuannhatrang/spot-instance-training-operator:v1.0.0
```

Using latest:
```bash
make deploy IMG=docker.io/lehuannhatrang/spot-instance-training-operator:latest
```

### 3. Verify Deployment

```bash
# Check if operator is running
kubectl get pods -n spot-instance-training-operator-system

# Check logs
kubectl logs -n spot-instance-training-operator-system \
  deployment/spot-instance-training-operator-controller-manager -f
```

## Creating Resources

### 1. Install Checkpoint Agent

First, ensure the checkpoint agent is installed on your nodes from:
https://github.com/lehuannhatrang/stateful-migration-operator

### 2. Create Secrets

```bash
# GCP credentials
kubectl create secret generic gcp-credentials \
  --from-file=credentials.json=/path/to/gcp-credentials.json \
  -n default

# Kubeadm join token (if using automatic node joining)
kubectl create secret generic kubeadm-join-token \
  --from-literal=token=<token> \
  --from-literal=ca-cert-hash=<hash> \
  --from-literal=control-plane-endpoint=<endpoint> \
  -n default
```

### 3. Apply Sample Resources

```bash
# Create SpotInstanceVM
kubectl apply -f config/samples/training_v1alpha1_spotinstancevm.yaml

# Create SpotInstanceJob
kubectl apply -f config/samples/training_v1alpha1_spotinstancejob.yaml
```

## Updating the Operator

To update a running operator:

```bash
# Build and push new version
./build-and-push.sh v1.1.0

# Update the deployment
kubectl set image deployment/spot-instance-training-operator-controller-manager \
  -n spot-instance-training-operator-system \
  manager=docker.io/lehuannhatrang/spot-instance-training-operator:v1.1.0
```

Or redeploy:
```bash
make deploy IMG=docker.io/lehuannhatrang/spot-instance-training-operator:v1.1.0
```

## Uninstalling

```bash
# Remove operator deployment
make undeploy

# Remove CRDs (this will delete all custom resources!)
make uninstall
```

## Troubleshooting

### Docker Login Issues

If you get authentication errors:
```bash
docker logout
docker login
# Enter your Docker Hub credentials
```

### Build Failures

If the build fails:
```bash
# Clean and rebuild
make clean
go mod tidy
make build
```

### Image Pull Errors

If Kubernetes can't pull the image:
1. Verify the image exists on Docker Hub
2. Check if the image is public or create an image pull secret
3. Verify the image name and tag in deployment

### Creating Image Pull Secret (if image is private)

```bash
kubectl create secret docker-registry dockerhub-secret \
  --docker-server=docker.io \
  --docker-username=lehuannhatrang \
  --docker-password=<password> \
  --docker-email=<email>
```

Then add to the deployment's service account.

## Development Workflow

For local development and testing:

```bash
# Run operator locally (not in cluster)
make run

# Run tests
make test

# Generate manifests after API changes
make manifests

# Install CRDs to your cluster
make install
```

## Notes

- The checkpoint manager is **NOT** included in this operator
- The operator integrates with the external checkpoint agent via CheckpointBackup CRs
- Ensure the checkpoint agent is running on nodes before using checkpoint features
- The operator requires RBAC permissions for CheckpointBackup resources

