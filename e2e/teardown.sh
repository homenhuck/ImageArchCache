#!/bin/bash
set -euo pipefail

CLUSTER_NAME="${CLUSTER_NAME:-arch-webhook-e2e}"

echo "Deleting kind cluster: $CLUSTER_NAME"
kind delete cluster --name "$CLUSTER_NAME"
echo "Done."
