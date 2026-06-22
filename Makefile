IMAGE_REPO ?= 752575507917.dkr.ecr.us-east-1.amazonaws.com/image-arch-webhook
IMAGE_TAG ?= v1.0.0
NAMESPACE ?= image-arch-system
KIND_CLUSTER ?= arch-webhook-e2e

.PHONY: build push deploy test test-unit test-e2e e2e-setup e2e-teardown clean logs cache

# --- Build & Deploy ---

build:
	docker build -t $(IMAGE_REPO):$(IMAGE_TAG) .

push: build
	docker push $(IMAGE_REPO):$(IMAGE_TAG)

deploy:
	helm upgrade --install image-arch-webhook ./deploy/helm \
		--set image.repository=$(IMAGE_REPO) \
		--set image.tag=$(IMAGE_TAG) \
		--set namespace=$(NAMESPACE)

uninstall:
	helm uninstall image-arch-webhook

# --- Unit Tests ---

test-unit:
	go test -v ./pkg/...

# --- E2E Tests ---

e2e-setup:
	@chmod +x ./e2e/setup.sh
	CLUSTER_NAME=$(KIND_CLUSTER) IMAGE_REPO=$(IMAGE_REPO) IMAGE_TAG=e2e ./e2e/setup.sh

e2e-teardown:
	@chmod +x ./e2e/teardown.sh
	CLUSTER_NAME=$(KIND_CLUSTER) ./e2e/teardown.sh

test-e2e: 
	go test -tags=e2e -v -timeout=120s ./e2e/

# Full test cycle: setup kind → run e2e → teardown
test-e2e-full: e2e-setup test-e2e e2e-teardown

# --- All Tests ---

test: test-unit

# --- Quick Validation (single pod test) ---

test-quick:
	@echo "Deploying test pod (amd64-only image)..."
	kubectl run test-amd64 --image=gcr.io/google_containers/pause-amd64:3.1 --restart=Never
	@sleep 3
	@echo "=== nodeSelector ==="
	@kubectl get pod test-amd64 -o jsonpath='{.spec.nodeSelector}' && echo
	@echo "=== annotation ==="
	@kubectl get pod test-amd64 -o jsonpath='{.metadata.annotations.arch-injector\.io/injected-arch}' && echo
	@echo ""
	@echo "Deploying test pod (multi-arch image)..."
	kubectl run test-multiarch --image=registry.k8s.io/pause:3.9 --restart=Never
	@sleep 3
	@echo "=== nodeSelector (should be empty) ==="
	@kubectl get pod test-multiarch -o jsonpath='{.spec.nodeSelector}' && echo

test-quick-clean:
	kubectl delete pod test-amd64 test-multiarch --ignore-not-found

# --- Observability ---

logs:
	kubectl logs -n $(NAMESPACE) -l app.kubernetes.io/name=image-arch-webhook -f

cache:
	kubectl get configmap -n $(NAMESPACE) image-arch-cache -o yaml

cache-count:
	@kubectl get configmap -n $(NAMESPACE) image-arch-cache -o json | \
		jq '.data | length' | xargs -I{} echo "Cached images: {}"
