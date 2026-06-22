#!/bin/bash
set -euo pipefail

# E2E Test Setup Script
# Creates a local kind cluster with the webhook installed for testing

CLUSTER_NAME="${CLUSTER_NAME:-arch-webhook-e2e}"
IMAGE_REPO="${IMAGE_REPO:-image-arch-webhook}"
IMAGE_TAG="${IMAGE_TAG:-e2e}"

echo "=== Image Architecture Webhook - E2E Test Setup ==="

# 1. Create kind cluster (if not exists)
if ! kind get clusters 2>/dev/null | grep -q "$CLUSTER_NAME"; then
    echo "Creating kind cluster: $CLUSTER_NAME"
    cat <<EOF | kind create cluster --name "$CLUSTER_NAME" --config=-
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
- role: control-plane
- role: worker
  # Simulate mixed arch via labels (kind only runs on host arch,
  # but we test the webhook logic regardless)
  extraPortMappings: []
EOF
else
    echo "Cluster $CLUSTER_NAME already exists"
fi

# 2. Build and load webhook image into kind
echo "Building webhook image..."
docker build -t "${IMAGE_REPO}:${IMAGE_TAG}" ..
kind load docker-image "${IMAGE_REPO}:${IMAGE_TAG}" --name "$CLUSTER_NAME"

# 3. Install cert-manager (required for webhook TLS)
echo "Installing cert-manager..."
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.14.0/cert-manager.yaml
echo "Waiting for cert-manager to be ready..."
kubectl wait --for=condition=Available deployment/cert-manager -n cert-manager --timeout=120s
kubectl wait --for=condition=Available deployment/cert-manager-webhook -n cert-manager --timeout=120s
sleep 10  # extra wait for webhook to be fully operational

# 4. Install the image-arch-webhook via Helm
echo "Installing image-arch-webhook..."
helm upgrade --install image-arch-webhook ../deploy/helm \
    --set image.repository="${IMAGE_REPO}" \
    --set image.tag="${IMAGE_TAG}" \
    --set image.pullPolicy=Never \
    --wait --timeout=120s

# 5. Verify webhook is running
echo "Verifying webhook pods..."
kubectl get pods -n image-arch-system
kubectl wait --for=condition=Ready pods -l app.kubernetes.io/name=image-arch-webhook \
    -n image-arch-system --timeout=60s

echo ""
echo "=== Setup complete! ==="
echo "Run tests with: go test -tags=e2e -v ./e2e/"
echo "Teardown with: kind delete cluster --name $CLUSTER_NAME"
