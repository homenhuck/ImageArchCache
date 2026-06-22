# Image Architecture Webhook

A Kubernetes Mutating Admission Webhook that automatically injects `nodeSelector` constraints based on container image architecture support.

## Problem

When using Karpenter with mixed-architecture NodePools (amd64 + arm64), the kube-scheduler may place single-architecture pods on incompatible nodes because it doesn't inspect image manifests before scheduling.

## Solution

This webhook intercepts Pod creation, queries the container registry for each image's supported architectures, and injects `nodeSelector: kubernetes.io/arch: <arch>` when a single-arch image is detected.

## Architecture

```
Pod CREATE/UPDATE → API Server → Mutating Webhook
                                        │
                                        ├─ Check L1 cache (in-memory)
                                        ├─ Check L2 cache (ConfigMap/etcd)
                                        └─ Query registry manifest (cache miss)
                                               │
                                               ▼
                                        Inject nodeSelector (JSON Patch)
```

## Features

- **Zero external dependencies** — no Redis, no database; uses Kubernetes ConfigMap as persistent cache
- **Two-level cache** — in-memory (L1) + ConfigMap/etcd (L2) for fast lookups
- **Registry-aware** — supports ECR, GCR, DockerHub, and any OCI-compliant registry
- **Safe by default** — `failurePolicy: Ignore` means pods are never blocked if the webhook is unavailable
- **Opt-out support** — skip via annotation (`arch-injector.io/skip: "true"`) or namespace label (`arch-injection: disabled`)
- **Conflict detection** — logs warnings if containers require conflicting architectures
- **Multi-container aware** — checks ALL containers (including init containers)

## Quick Start

### Prerequisites

- Kubernetes 1.25+
- cert-manager (for TLS certificate management)
- Container registry credentials configured (ECR via IRSA, or imagePullSecrets)

### Install

```bash
# Build and push image
docker build -t YOUR_ECR_REPO/image-arch-webhook:v1.0.0 .
docker push YOUR_ECR_REPO/image-arch-webhook:v1.0.0

# Install via Helm
helm install image-arch-webhook ./deploy/helm \
  --set image.repository=YOUR_ECR_REPO/image-arch-webhook \
  --set image.tag=v1.0.0
```

### Verify

```bash
# Deploy a single-arch (amd64-only) image
kubectl apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: test-amd64
spec:
  containers:
  - name: test
    image: gcr.io/google_containers/pause-amd64:3.1
EOF

# Check if nodeSelector was injected
kubectl get pod test-amd64 -o jsonpath='{.spec.nodeSelector}'
# Expected: {"kubernetes.io/arch":"amd64"}

# Check annotation
kubectl get pod test-amd64 -o jsonpath='{.metadata.annotations.arch-injector\.io/injected-arch}'
# Expected: amd64
```

### Opt-out

```yaml
# Per-pod: add annotation
metadata:
  annotations:
    arch-injector.io/skip: "true"

# Per-namespace: add label
metadata:
  labels:
    arch-injection: disabled
```

## Configuration

| Flag | Default | Description |
|------|---------|-------------|
| `--port` | 8443 | Webhook HTTPS port |
| `--cache-ttl` | 24h | Cache entry expiration |
| `--cache-namespace` | image-arch-system | Namespace for cache ConfigMap |
| `--cache-configmap` | image-arch-cache | ConfigMap name |
| `--metrics-port` | 8080 | Health/metrics port |

## ECR Authentication (IRSA)

For private ECR images, configure IRSA on the ServiceAccount:

```yaml
serviceAccount:
  annotations:
    eks.amazonaws.com/role-arn: arn:aws:iam::ACCOUNT:role/image-arch-webhook-ecr
```

Required IAM policy:
```json
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Action": [
      "ecr:GetDownloadUrlForLayer",
      "ecr:BatchGetImage",
      "ecr:GetAuthorizationToken"
    ],
    "Resource": "*"
  }]
}
```

## How It Works

1. Pod creation request arrives at the API server
2. API server calls our mutating webhook
3. Webhook checks if `nodeSelector` or `nodeAffinity` for `kubernetes.io/arch` already exists → skip if yes
4. For each container image:
   - Check L1 (memory) → L2 (ConfigMap) cache
   - On cache miss: query registry for manifest index
   - Parse supported architectures from manifest
5. If any image is single-arch → inject `nodeSelector` via JSON Patch
6. If all images are multi-arch → allow without mutation
7. If containers have conflicting arch requirements → allow without mutation (let it fail naturally with clear error)

## License

Apache 2.0
