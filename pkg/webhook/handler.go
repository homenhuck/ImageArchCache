package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	"github.com/image-arch-webhook/pkg/cache"
	"github.com/image-arch-webhook/pkg/registry"
)

const (
	// Annotation to skip injection
	AnnotationSkip = "arch-injector.io/skip"
	// Annotation showing what was injected
	AnnotationInjected = "arch-injector.io/injected-arch"
)

// Handler processes admission requests
type Handler struct {
	cache    *cache.ArchCache
	registry *registry.Client
}

// NewHandler creates a new webhook handler
func NewHandler(cache *cache.ArchCache, registry *registry.Client) *Handler {
	return &Handler{
		cache:    cache,
		registry: registry,
	}
}

// ServeMutate handles the /mutate endpoint
func (h *Handler) ServeMutate(w http.ResponseWriter, r *http.Request) {
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

// mutate is the core mutation logic
func (h *Handler) mutate(ctx context.Context, req *admissionv1.AdmissionRequest) *admissionv1.AdmissionResponse {
	// Parse the Pod
	var pod corev1.Pod
	if err := json.Unmarshal(req.Object.Raw, &pod); err != nil {
		klog.Errorf("Failed to unmarshal pod: %v", err)
		return allowResponse("failed to unmarshal pod")
	}

	// Check skip annotation
	if pod.Annotations != nil && pod.Annotations[AnnotationSkip] == "true" {
		klog.V(2).Infof("Skipping pod %s/%s (annotation)", pod.Namespace, pod.Name)
		return allowResponse("skipped by annotation")
	}

	// Check if nodeSelector for arch already exists
	if pod.Spec.NodeSelector != nil {
		if _, exists := pod.Spec.NodeSelector["kubernetes.io/arch"]; exists {
			klog.V(2).Infof("Skipping pod %s/%s (nodeSelector already set)", pod.Namespace, pod.Name)
			return allowResponse("arch nodeSelector already present")
		}
	}

	// Check if nodeAffinity for arch already exists
	if hasArchAffinity(&pod) {
		klog.V(2).Infof("Skipping pod %s/%s (nodeAffinity already set)", pod.Namespace, pod.Name)
		return allowResponse("arch nodeAffinity already present")
	}

	// Collect architectures from all containers
	allArchs, err := h.resolveArchitectures(ctx, &pod)
	if err != nil {
		klog.Warningf("Failed to resolve architectures for pod %s/%s: %v", pod.Namespace, pod.Name, err)
		return allowResponse(fmt.Sprintf("registry lookup failed: %v", err))
	}

	// Determine if we need to inject
	// If ALL images support the same single arch set → inject
	// If all are multi-arch → no injection needed
	targetArch := determineTargetArch(allArchs)
	if targetArch == "" {
		klog.V(2).Infof("Pod %s/%s: all images are multi-arch, no injection needed", pod.Namespace, pod.Name)
		return allowResponse("multi-arch images, no injection needed")
	}

	// Build JSON patch to inject nodeSelector
	patches := buildPatches(&pod, targetArch)
	if len(patches) == 0 {
		return allowResponse("no patches needed")
	}

	patchBytes, err := json.Marshal(patches)
	if err != nil {
		klog.Errorf("Failed to marshal patches: %v", err)
		return allowResponse("failed to build patches")
	}

	klog.Infof("Injecting nodeSelector kubernetes.io/arch=%s for pod %s/%s",
		targetArch, req.Namespace, pod.GenerateName)

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

// resolveArchitectures gets the supported architectures for all containers in the pod
func (h *Handler) resolveArchitectures(ctx context.Context, pod *corev1.Pod) (map[string][]string, error) {
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

// determineTargetArch analyzes all container architectures and returns:
// - "" if all images are multi-arch (no constraint needed)
// - "amd64" or "arm64" if any image is single-arch (constraint required)
// - "" with error log if images have conflicting single-arch requirements
func determineTargetArch(imageArchs map[string][]string) string {
	var singleArchConstraint string

	for image, archs := range imageArchs {
		if len(archs) == 1 {
			// Single-arch image found
			if singleArchConstraint == "" {
				singleArchConstraint = archs[0]
			} else if singleArchConstraint != archs[0] {
				// Conflict: one container needs amd64, another needs arm64
				klog.Warningf("Conflicting arch requirements: %s needs %s, but another needs %s",
					image, archs[0], singleArchConstraint)
				return "" // Can't resolve — let it fail naturally
			}
		} else {
			// Multi-arch — check if it includes the required single arch
			if singleArchConstraint != "" {
				found := false
				for _, a := range archs {
					if a == singleArchConstraint {
						found = true
						break
					}
				}
				if !found {
					klog.Warningf("Multi-arch image %s doesn't support required arch %s",
						image, singleArchConstraint)
					return ""
				}
			}
		}
	}

	return singleArchConstraint
}

// buildPatches creates the JSON patch operations
func buildPatches(pod *corev1.Pod, arch string) []jsonPatch {
	var patches []jsonPatch

	// Add nodeSelector if not present
	if pod.Spec.NodeSelector == nil {
		patches = append(patches, jsonPatch{
			Op:    "add",
			Path:  "/spec/nodeSelector",
			Value: map[string]string{"kubernetes.io/arch": arch},
		})
	} else {
		patches = append(patches, jsonPatch{
			Op:    "add",
			Path:  "/spec/nodeSelector/kubernetes.io~1arch",
			Value: arch,
		})
	}

	// Add annotation to indicate injection happened
	if pod.Annotations == nil {
		patches = append(patches, jsonPatch{
			Op:   "add",
			Path: "/metadata/annotations",
			Value: map[string]string{
				AnnotationInjected: arch,
			},
		})
	} else {
		patches = append(patches, jsonPatch{
			Op:    "add",
			Path:  fmt.Sprintf("/metadata/annotations/%s", escapeJSONPointer(AnnotationInjected)),
			Value: arch,
		})
	}

	return patches
}

// hasArchAffinity checks if the pod already has a nodeAffinity for kubernetes.io/arch
func hasArchAffinity(pod *corev1.Pod) bool {
	if pod.Spec.Affinity == nil || pod.Spec.Affinity.NodeAffinity == nil {
		return false
	}
	na := pod.Spec.Affinity.NodeAffinity

	// Check required scheduling terms
	if na.RequiredDuringSchedulingIgnoredDuringExecution != nil {
		for _, term := range na.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
			for _, expr := range term.MatchExpressions {
				if expr.Key == "kubernetes.io/arch" {
					return true
				}
			}
		}
	}

	// Check preferred scheduling terms
	for _, pref := range na.PreferredDuringSchedulingIgnoredDuringExecution {
		for _, expr := range pref.Preference.MatchExpressions {
			if expr.Key == "kubernetes.io/arch" {
				return true
			}
		}
	}

	return false
}

// allowResponse creates a simple allow response (no mutation)
func allowResponse(message string) *admissionv1.AdmissionResponse {
	return &admissionv1.AdmissionResponse{
		Allowed: true,
		Result: &metav1.Status{
			Message: message,
		},
	}
}

// jsonPatch represents a RFC 6902 JSON Patch operation
type jsonPatch struct {
	Op    string      `json:"op"`
	Path  string      `json:"path"`
	Value interface{} `json:"value,omitempty"`
}

// escapeJSONPointer escapes special characters for JSON Pointer (RFC 6901)
func escapeJSONPointer(s string) string {
	s = strings.ReplaceAll(s, "~", "~0")
	s = strings.ReplaceAll(s, "/", "~1")
	return s
}
