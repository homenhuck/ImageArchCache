package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// --- Mock implementations ---

type mockCacheImpl struct {
	data map[string][]string
}

func newMockCache(data map[string][]string) *mockCacheImpl {
	return &mockCacheImpl{data: data}
}

func (m *mockCacheImpl) Get(ctx context.Context, imageRef string) ([]string, bool) {
	if archs, ok := m.data[imageRef]; ok {
		return archs, true
	}
	return nil, false
}

func (m *mockCacheImpl) Set(ctx context.Context, imageRef string, archs []string, digest string) {
	m.data[imageRef] = archs
}

type mockRegistryImpl struct {
	results map[string]*ArchResult
}

func newMockRegistry(results map[string]*ArchResult) *mockRegistryImpl {
	return &mockRegistryImpl{results: results}
}

func (m *mockRegistryImpl) GetArchitectures(ctx context.Context, imageRef string) (*ArchResult, error) {
	if r, ok := m.results[imageRef]; ok {
		return r, nil
	}
	// Default: multi-arch
	return &ArchResult{
		Image:         imageRef,
		Digest:        "sha256:unknown",
		Architectures: []string{"amd64", "arm64"},
	}, nil
}

// --- Unit Tests ---

func TestDetermineTargetArch(t *testing.T) {
	tests := []struct {
		name       string
		imageArchs map[string][]string
		wantArch   string
	}{
		{
			name: "all multi-arch → no injection",
			imageArchs: map[string][]string{
				"nginx:latest":              {"amd64", "arm64"},
				"registry.k8s.io/pause:3.8": {"amd64", "arm64"},
			},
			wantArch: "",
		},
		{
			name: "single-arch amd64 → inject amd64",
			imageArchs: map[string][]string{
				"gcr.io/google_containers/pause-amd64:3.1": {"amd64"},
			},
			wantArch: "amd64",
		},
		{
			name: "single-arch arm64 → inject arm64",
			imageArchs: map[string][]string{
				"my-registry/app-arm64:latest": {"arm64"},
			},
			wantArch: "arm64",
		},
		{
			name: "mixed: one single-arch + one multi-arch → inject constraint",
			imageArchs: map[string][]string{
				"gcr.io/google_containers/pause-amd64:3.1": {"amd64"},
				"registry.k8s.io/pause:3.8":                {"amd64", "arm64"},
			},
			wantArch: "amd64",
		},
		{
			name: "conflict: amd64 vs arm64 → no injection (irreconcilable)",
			imageArchs: map[string][]string{
				"my-registry/app-amd64:latest": {"amd64"},
				"my-registry/app-arm64:latest": {"arm64"},
			},
			wantArch: "",
		},
		{
			name: "single image with 3 archs → multi-arch, no injection",
			imageArchs: map[string][]string{
				"my-registry/app:latest": {"amd64", "arm64", "s390x"},
			},
			wantArch: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := determineTargetArch(tt.imageArchs)
			if got != tt.wantArch {
				t.Errorf("determineTargetArch() = %q, want %q", got, tt.wantArch)
			}
		})
	}
}

func TestHasArchAffinity(t *testing.T) {
	tests := []struct {
		name string
		pod  *corev1.Pod
		want bool
	}{
		{
			name: "no affinity at all",
			pod:  &corev1.Pod{},
			want: false,
		},
		{
			name: "required nodeAffinity for arch",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Affinity: &corev1.Affinity{
						NodeAffinity: &corev1.NodeAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
								NodeSelectorTerms: []corev1.NodeSelectorTerm{{
									MatchExpressions: []corev1.NodeSelectorRequirement{{
										Key:      "kubernetes.io/arch",
										Operator: corev1.NodeSelectorOpIn,
										Values:   []string{"amd64"},
									}},
								}},
							},
						},
					},
				},
			},
			want: true,
		},
		{
			name: "preferred nodeAffinity for arch",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Affinity: &corev1.Affinity{
						NodeAffinity: &corev1.NodeAffinity{
							PreferredDuringSchedulingIgnoredDuringExecution: []corev1.PreferredSchedulingTerm{{
								Weight: 100,
								Preference: corev1.NodeSelectorTerm{
									MatchExpressions: []corev1.NodeSelectorRequirement{{
										Key:      "kubernetes.io/arch",
										Operator: corev1.NodeSelectorOpIn,
										Values:   []string{"arm64"},
									}},
								},
							}},
						},
					},
				},
			},
			want: true,
		},
		{
			name: "nodeAffinity for different key (zone) → false",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Affinity: &corev1.Affinity{
						NodeAffinity: &corev1.NodeAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
								NodeSelectorTerms: []corev1.NodeSelectorTerm{{
									MatchExpressions: []corev1.NodeSelectorRequirement{{
										Key:      "topology.kubernetes.io/zone",
										Operator: corev1.NodeSelectorOpIn,
										Values:   []string{"us-east-1a"},
									}},
								}},
							},
						},
					},
				},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasArchAffinity(tt.pod); got != tt.want {
				t.Errorf("hasArchAffinity() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBuildPatches(t *testing.T) {
	tests := []struct {
		name        string
		pod         *corev1.Pod
		arch        string
		wantPatches int
		checkPath   string
		checkValue  interface{}
	}{
		{
			name:        "no existing nodeSelector → add map",
			pod:         &corev1.Pod{},
			arch:        "amd64",
			wantPatches: 2,
			checkPath:   "/spec/nodeSelector",
		},
		{
			name: "existing nodeSelector → add key",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					NodeSelector: map[string]string{"disktype": "ssd"},
				},
			},
			arch:        "arm64",
			wantPatches: 2,
			checkPath:   "/spec/nodeSelector/kubernetes.io~1arch",
		},
		{
			name: "existing annotations → add annotation key",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{"foo": "bar"},
				},
			},
			arch:        "amd64",
			wantPatches: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			patches := buildPatches(tt.pod, tt.arch)
			if len(patches) != tt.wantPatches {
				t.Errorf("got %d patches, want %d: %+v", len(patches), tt.wantPatches, patches)
			}
		})
	}
}

// --- Integration Tests (full request → response) ---

func TestIntegration_SingleArchImage_InjectsNodeSelector(t *testing.T) {
	cache := newMockCache(map[string][]string{
		"gcr.io/google_containers/pause-amd64:3.1": {"amd64"},
	})
	reg := newMockRegistry(nil) // won't be called (cache hit)
	handler := NewTestableHandler(cache, reg)

	resp := sendAdmissionRequest(t, handler, corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-amd64", Namespace: "default"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app", Image: "gcr.io/google_containers/pause-amd64:3.1"},
			},
		},
	})

	assertAllowed(t, resp)
	assertPatchContains(t, resp, "/spec/nodeSelector", "amd64")
}

func TestIntegration_MultiArchImage_NoPatch(t *testing.T) {
	cache := newMockCache(map[string][]string{
		"registry.k8s.io/pause:3.8": {"amd64", "arm64"},
	})
	handler := NewTestableHandler(cache, newMockRegistry(nil))

	resp := sendAdmissionRequest(t, handler, corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-multi", Namespace: "default"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app", Image: "registry.k8s.io/pause:3.8"},
			},
		},
	})

	assertAllowed(t, resp)
	if resp.Patch != nil {
		t.Error("expected no patch for multi-arch image")
	}
}

func TestIntegration_SkipAnnotation_NoPatch(t *testing.T) {
	cache := newMockCache(map[string][]string{
		"gcr.io/google_containers/pause-amd64:3.1": {"amd64"},
	})
	handler := NewTestableHandler(cache, newMockRegistry(nil))

	resp := sendAdmissionRequest(t, handler, corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "test-skip",
			Namespace:   "default",
			Annotations: map[string]string{AnnotationSkip: "true"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app", Image: "gcr.io/google_containers/pause-amd64:3.1"},
			},
		},
	})

	assertAllowed(t, resp)
	if resp.Patch != nil {
		t.Error("expected no patch when skip annotation is present")
	}
}

func TestIntegration_ExistingNodeSelector_NoPatch(t *testing.T) {
	cache := newMockCache(map[string][]string{})
	handler := NewTestableHandler(cache, newMockRegistry(nil))

	resp := sendAdmissionRequest(t, handler, corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-existing", Namespace: "default"},
		Spec: corev1.PodSpec{
			NodeSelector: map[string]string{"kubernetes.io/arch": "amd64"},
			Containers: []corev1.Container{
				{Name: "app", Image: "anything:latest"},
			},
		},
	})

	assertAllowed(t, resp)
	if resp.Patch != nil {
		t.Error("expected no patch when nodeSelector already set")
	}
}

func TestIntegration_CacheMiss_QueriesRegistry(t *testing.T) {
	cache := newMockCache(map[string][]string{}) // empty cache
	reg := newMockRegistry(map[string]*ArchResult{
		"my-private-registry/app:v2.0": {
			Image:         "my-private-registry/app:v2.0",
			Digest:        "sha256:abc123",
			Architectures: []string{"arm64"},
		},
	})
	handler := NewTestableHandler(cache, reg)

	resp := sendAdmissionRequest(t, handler, corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-registry", Namespace: "default"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app", Image: "my-private-registry/app:v2.0"},
			},
		},
	})

	assertAllowed(t, resp)
	assertPatchContains(t, resp, "/spec/nodeSelector", "arm64")

	// Verify cache was populated
	archs, found := cache.Get(context.Background(), "my-private-registry/app:v2.0")
	if !found {
		t.Error("expected cache to be populated after registry query")
	}
	if len(archs) != 1 || archs[0] != "arm64" {
		t.Errorf("expected [arm64] in cache, got %v", archs)
	}
}

func TestIntegration_InitContainer_SingleArch(t *testing.T) {
	cache := newMockCache(map[string][]string{
		"my-registry/init-amd64:latest": {"amd64"},
		"nginx:latest":                  {"amd64", "arm64"},
	})
	handler := NewTestableHandler(cache, newMockRegistry(nil))

	resp := sendAdmissionRequest(t, handler, corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-init", Namespace: "default"},
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{
				{Name: "init", Image: "my-registry/init-amd64:latest"},
			},
			Containers: []corev1.Container{
				{Name: "app", Image: "nginx:latest"},
			},
		},
	})

	assertAllowed(t, resp)
	assertPatchContains(t, resp, "/spec/nodeSelector", "amd64")
}

func TestIntegration_ConflictingArchs_NoPatch(t *testing.T) {
	cache := newMockCache(map[string][]string{
		"app-amd64:latest": {"amd64"},
		"app-arm64:latest": {"arm64"},
	})
	handler := NewTestableHandler(cache, newMockRegistry(nil))

	resp := sendAdmissionRequest(t, handler, corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-conflict", Namespace: "default"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "a", Image: "app-amd64:latest"},
				{Name: "b", Image: "app-arm64:latest"},
			},
		},
	})

	assertAllowed(t, resp)
	if resp.Patch != nil {
		t.Error("expected no patch for conflicting architectures")
	}
}

// --- Test Helpers ---

func sendAdmissionRequest(t *testing.T, handler *TestableHandler, pod corev1.Pod) *admissionv1.AdmissionResponse {
	t.Helper()

	raw, _ := json.Marshal(pod)
	admReview := admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{APIVersion: "admission.k8s.io/v1", Kind: "AdmissionReview"},
		Request: &admissionv1.AdmissionRequest{
			UID:       "test-uid",
			Namespace: pod.Namespace,
			Object:    runtime.RawExtension{Raw: raw},
		},
	}

	body, _ := json.Marshal(admReview)
	req := httptest.NewRequest(http.MethodPost, "/mutate", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.ServeMutate(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("HTTP %d: %s", w.Code, w.Body.String())
	}

	var resp admissionv1.AdmissionReview
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	return resp.Response
}

func assertAllowed(t *testing.T, resp *admissionv1.AdmissionResponse) {
	t.Helper()
	if !resp.Allowed {
		t.Fatalf("expected Allowed=true, got false (message: %s)", resp.Result.Message)
	}
}

func assertPatchContains(t *testing.T, resp *admissionv1.AdmissionResponse, path, arch string) {
	t.Helper()
	if resp.Patch == nil {
		t.Fatal("expected patch but got nil")
	}

	var patches []jsonPatch
	if err := json.Unmarshal(resp.Patch, &patches); err != nil {
		t.Fatalf("unmarshal patch: %v", err)
	}

	for _, p := range patches {
		if p.Path == path {
			// Check if value contains the arch
			switch v := p.Value.(type) {
			case map[string]interface{}:
				if v["kubernetes.io/arch"] == arch {
					return
				}
			case string:
				if v == arch {
					return
				}
			}
		}
		// Also check the key-path variant
		if p.Path == "/spec/nodeSelector/kubernetes.io~1arch" && p.Value == arch {
			return
		}
	}

	t.Errorf("patch for path=%q arch=%q not found in: %s", path, arch, string(resp.Patch))
}
