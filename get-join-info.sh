#!/bin/bash
# Script to get kubeadm join information

echo "==================================="
echo "Getting Kubeadm Join Information"
echo "==================================="

# Generate token
echo "Generating token..."
TOKEN=$(kubeadm token create)

# Get CA cert hash
echo "Getting CA cert hash..."
CA_HASH=$(openssl x509 -pubkey -in /etc/kubernetes/pki/ca.crt | \
    openssl rsa -pubin -outform der 2>/dev/null | \
    openssl dgst -sha256 -hex | sed 's/^.* //')

# Get control plane endpoint
CONTROL_PLANE=$(kubectl cluster-info | grep "control plane" | awk '{print $NF}' | sed 's/https:\/\///' | sed 's/\x1b\[[0-9;]*m//g')

echo ""
echo "==================================="
echo "Kubeadm Join Information:"
echo "==================================="
echo "Token: $TOKEN"
echo "CA Cert Hash: sha256:$CA_HASH"
echo "Control Plane: $CONTROL_PLANE"
echo ""
echo "==================================="
echo "Creating Kubernetes Secret:"
echo "==================================="

kubectl create secret generic kubeadm-join-token \
  --from-literal=token=$TOKEN \
  --from-literal=ca-cert-hash=sha256:$CA_HASH \
  --from-literal=control-plane-endpoint=$CONTROL_PLANE \
  --dry-run=client -o yaml | kubectl apply -f -

echo ""
echo "✅ Secret created!"
echo ""
echo "==================================="
echo "Update your spotinstance-vm.yaml:"
echo "==================================="
echo "kubeadmJoinConfig:"
echo "  tokenSecretRef: \"kubeadm-join-token\""
echo "  caCertHash: \"sha256:$CA_HASH\""
echo "  controlPlaneEndpoint: \"$CONTROL_PLANE\""
echo "==================================="

