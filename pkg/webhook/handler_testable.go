package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
)

// TestableHandler uses interfaces for cache and registry (testable version)
type TestableHandler struct {
	cache    CacheInterface
	registry RegistryInterface
}

// NewTestableHandler creates a handler with injected dependencies
func NewTestableHandler(cache CacheInterface, registry RegistryInterface) *TestableHandler {
	return &TestableHandler{
		cache:    cache,
		registry: registry,
	}
}

// ServeMutate handles the /mutate endpoint
func (h *TestableHandler) ServeMutate(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()

	body, err := io.ReadAll(r.Body)
	if err != nil {
		klog.Errorf("Failed to read request body: %v", err)
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}

	var admissionReview admissionv1.AdmissionReview
	if err := json.Unmarshal(body, &admissionReview); err != nil {
		klog.Errorf("Failed to unmarshal admission review: %v", err)
		http.Error(w, "failed to unmarshal", http.StatusBadRequest)
		return
	}

	response := h.mutate(r.Context(), admissionReview.Request)
	admissionReview.Response = response
	admissionReview.Response.UID = admissionReview.Request.UID

	respBytes, err := json.Marshal(admissionReview)
	if err != nil {
		klog.Errorf("Failed to marshal response: %v", err)
		http.Error(w, "failed to marshal", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write(respBytes)
	klog.V(2).Infof("Processed admission request in %v", time.Since(startTime))
}

func (h *TestableHandler) mutate(ctx context.Context, req *admissionv1.AdmissionRequest) *admissionv1.AdmissionResponse {
	var pod corev1.Pod
	if err := json.Unmarshal(req.Object.Raw, &pod); err != nil {
		return allowResponse("failed to unmarshal pod")
	}

	// Check skip annotation
	if pod.Annotations != nil && pod.Annotations[AnnotationSkip] == "true" {
		return allowResponse("skipped by annotation")
	}

	// Check if nodeSelector for arch already exists
	if pod.Spec.NodeSelector != nil {
		if _, exists := pod.Spec.NodeSelector["kubernetes.io/arch"]; exists {
			return allowResponse("arch nodeSelector already present")
		}
	}

	// Check if nodeAffinity for arch already exists
	if hasArchAffinity(&pod) {
		return allowResponse("arch nodeAffinity already present")
	}

	// Collect architectures from all containers
	allArchs, err := h.resolveArchitectures(ctx, &pod)
	if err != nil {
		return allowResponse(fmt.Sprintf("registry lookup failed: %v", err))
	}

	targetArch := determineTargetArch(allArchs)
	if targetArch == "" {
		return allowResponse("multi-arch images, no injection needed")
	}

	patches := buildPatches(&pod, targetArch)
	if len(patches) == 0 {
		return allowResponse("no patches needed")
	}

	patchBytes, err := json.Marshal(patches)
	if err != nil {
		return allowResponse("failed to build patches")
	}

	klog.Infof("Injecting nodeSelector kubernetes.io/arch=%s for pod %s/%s",
		targetArch, req.Namespace, pod.Name)

	patchType := admissionv1.PatchTypeJSONPatch
	return &admissionv1.AdmissionResponse{
		Allowed:   true,
		PatchType: &patchType,
		Patch:     patchBytes,
		Result: &metav1.Status{
			Message: fmt.Sprintf("injected arch constraint: %s", targetArch),
		},
	}
}

func (h *TestableHandler) resolveArchitectures(ctx context.Context, pod *corev1.Pod) (map[string][]string, error) {
	result := make(map[string][]string)

	containers := append(pod.Spec.InitContainers, pod.Spec.Containers...)
	for _, c := range containers {
		if c.Image == "" {
			continue
		}

		// Check cache first
		archs, found := h.cache.Get(ctx, c.Image)
		if found {
			result[c.Image] = archs
			continue
		}

		// Query registry
		archResult, err := h.registry.GetArchitectures(ctx, c.Image)
		if err != nil {
			return nil, fmt.Errorf("image %s: %w", c.Image, err)
		}

		// Store in cache
		h.cache.Set(ctx, c.Image, archResult.Architectures, archResult.Digest)
		result[c.Image] = archResult.Architectures
	}

	return result, nil
}
