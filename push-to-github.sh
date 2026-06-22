#!/bin/bash
set -euo pipefail

# Push image-arch-webhook to GitHub
# Run this script from the image-arch-webhook/ directory

echo "=== Pushing to https://github.com/homenhuck/ImageArchCache.git ==="

git init
git add .
git commit -m "feat: Image Architecture Webhook - auto-inject nodeSelector based on OCI manifest

Kubernetes Mutating Admission Webhook that automatically injects
nodeSelector constraints based on container image architecture support.

Solves the problem where kube-scheduler places single-arch pods on
incompatible nodes in mixed-architecture clusters (Karpenter with
amd64 + arm64 NodePools).

Features:
- Registry manifest inspection (OCI/Docker manifest list)
- Two-level cache: in-memory (L1) + ConfigMap/etcd (L2)
- Supports ECR, GCR, DockerHub, any OCI registry
- failurePolicy: Ignore (safe by default)
- Opt-out via annotation or namespace label
- Multi-container aware (init + app containers)
- Full Helm chart + e2e tests"

git branch -M main
git remote add origin https://github.com/homenhuck/ImageArchCache.git
git push -u origin main

echo ""
echo "✅ Done! https://github.com/homenhuck/ImageArchCache"
