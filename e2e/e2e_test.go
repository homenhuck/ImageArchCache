// +build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

var clientset *kubernetes.Clientset

func TestMain(m *testing.M) {
	// Use KUBECONFIG or default ~/.kube/config
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		kubeconfig = os.Getenv("HOME") + "/.kube/config"
	}

	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to build kubeconfig: %v\n", err)
		os.Exit(1)
	}

	clientset, err = kubernetes.NewForConfig(config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create clientset: %v\n", err)
		os.Exit(1)
	}

	os.Exit(m.Run())
}

// TestE2E_SingleArchAMD64_InjectsNodeSelector deploys a single-arch amd64 pod
// and verifies the webhook injects kubernetes.io/arch=amd64 nodeSelector
func TestE2E_SingleArchAMD64_InjectsNodeSelector(t *testing.T) {
	ctx := context.Background()
	ns := "default"
	podName := "e2e-test-amd64-" + randomSuffix()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: ns,
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "app",
					Image: "gcr.io/google_containers/pause-amd64:3.1",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    mustParseQuantity("10m"),
							corev1.ResourceMemory: mustParseQuantity("16Mi"),
						},
					},
				},
			},
			RestartPolicy: corev1.RestartPolicyNever,
		},
	}

	// Create pod
	created, err := clientset.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Failed to create pod: %v", err)
	}
	defer cleanupPod(t, ctx, ns, podName)

	// Verify nodeSelector was injected
	if created.Spec.NodeSelector == nil {
		t.Fatal("Expected nodeSelector to be injected, got nil")
	}
	arch, ok := created.Spec.NodeSelector["kubernetes.io/arch"]
	if !ok {
		t.Fatalf("Expected kubernetes.io/arch in nodeSelector, got: %v", created.Spec.NodeSelector)
	}
	if arch != "amd64" {
		t.Errorf("Expected arch=amd64, got %q", arch)
	}

	// Verify annotation
	injected, ok := created.Annotations["arch-injector.io/injected-arch"]
	if !ok {
		t.Error("Expected arch-injector.io/injected-arch annotation")
	} else if injected != "amd64" {
		t.Errorf("Expected annotation value=amd64, got %q", injected)
	}

	t.Logf("✅ Pod %s: nodeSelector injected correctly (arch=amd64)", podName)
}

// TestE2E_MultiArchImage_NoInjection deploys a multi-arch image
// and verifies NO nodeSelector is injected
func TestE2E_MultiArchImage_NoInjection(t *testing.T) {
	ctx := context.Background()
	ns := "default"
	podName := "e2e-test-multiarch-" + randomSuffix()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: ns,
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "app",
					Image: "registry.k8s.io/pause:3.9",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    mustParseQuantity("10m"),
							corev1.ResourceMemory: mustParseQuantity("16Mi"),
						},
					},
				},
			},
			RestartPolicy: corev1.RestartPolicyNever,
		},
	}

	created, err := clientset.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Failed to create pod: %v", err)
	}
	defer cleanupPod(t, ctx, ns, podName)

	// Verify NO arch nodeSelector was injected
	if created.Spec.NodeSelector != nil {
		if _, ok := created.Spec.NodeSelector["kubernetes.io/arch"]; ok {
			t.Error("Expected NO kubernetes.io/arch nodeSelector for multi-arch image")
		}
	}

	// Verify NO annotation
	if _, ok := created.Annotations["arch-injector.io/injected-arch"]; ok {
		t.Error("Expected NO arch-injector.io/injected-arch annotation for multi-arch image")
	}

	t.Logf("✅ Pod %s: no injection for multi-arch image (correct)", podName)
}

// TestE2E_SkipAnnotation verifies opt-out via annotation
func TestE2E_SkipAnnotation(t *testing.T) {
	ctx := context.Background()
	ns := "default"
	podName := "e2e-test-skip-" + randomSuffix()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: ns,
			Annotations: map[string]string{
				"arch-injector.io/skip": "true",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "app",
					Image: "gcr.io/google_containers/pause-amd64:3.1",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    mustParseQuantity("10m"),
							corev1.ResourceMemory: mustParseQuantity("16Mi"),
						},
					},
				},
			},
			RestartPolicy: corev1.RestartPolicyNever,
		},
	}

	created, err := clientset.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Failed to create pod: %v", err)
	}
	defer cleanupPod(t, ctx, ns, podName)

	// With skip annotation, no injection should happen
	if created.Spec.NodeSelector != nil {
		if _, ok := created.Spec.NodeSelector["kubernetes.io/arch"]; ok {
			t.Error("Expected NO nodeSelector when skip annotation is set")
		}
	}

	t.Logf("✅ Pod %s: skip annotation respected (no injection)", podName)
}

// TestE2E_ExistingNodeSelector_NoOverride verifies we don't override existing arch constraint
func TestE2E_ExistingNodeSelector_NoOverride(t *testing.T) {
	ctx := context.Background()
	ns := "default"
	podName := "e2e-test-existing-" + randomSuffix()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: ns,
		},
		Spec: corev1.PodSpec{
			NodeSelector: map[string]string{
				"kubernetes.io/arch": "arm64", // pre-set to arm64 intentionally
			},
			Containers: []corev1.Container{
				{
					Name:  "app",
					Image: "gcr.io/google_containers/pause-amd64:3.1", // amd64-only image
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    mustParseQuantity("10m"),
							corev1.ResourceMemory: mustParseQuantity("16Mi"),
						},
					},
				},
			},
			RestartPolicy: corev1.RestartPolicyNever,
		},
	}

	created, err := clientset.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Failed to create pod: %v", err)
	}
	defer cleanupPod(t, ctx, ns, podName)

	// Existing nodeSelector should NOT be overridden
	arch := created.Spec.NodeSelector["kubernetes.io/arch"]
	if arch != "arm64" {
		t.Errorf("Expected existing arm64 to be preserved, got %q", arch)
	}

	t.Logf("✅ Pod %s: existing nodeSelector preserved (arm64)", podName)
}

// TestE2E_NamespaceOptOut verifies namespace-level opt-out
func TestE2E_NamespaceOptOut(t *testing.T) {
	ctx := context.Background()
	nsName := "e2e-test-optout-" + randomSuffix()

	// Create namespace with opt-out label
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: nsName,
			Labels: map[string]string{
				"arch-injection": "disabled",
			},
		},
	}
	_, err := clientset.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Failed to create namespace: %v", err)
	}
	defer func() {
		clientset.CoreV1().Namespaces().Delete(ctx, nsName, metav1.DeleteOptions{})
	}()

	podName := "e2e-test-nsoptout"
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: nsName,
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "app",
					Image: "gcr.io/google_containers/pause-amd64:3.1",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    mustParseQuantity("10m"),
							corev1.ResourceMemory: mustParseQuantity("16Mi"),
						},
					},
				},
			},
			RestartPolicy: corev1.RestartPolicyNever,
		},
	}

	created, err := clientset.CoreV1().Pods(nsName).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Failed to create pod: %v", err)
	}

	// In opted-out namespace, no injection should happen
	if created.Spec.NodeSelector != nil {
		if _, ok := created.Spec.NodeSelector["kubernetes.io/arch"]; ok {
			t.Error("Expected NO nodeSelector in opted-out namespace")
		}
	}

	t.Logf("✅ Pod %s in ns %s: namespace opt-out respected", podName, nsName)
}

// TestE2E_CachePopulated verifies the ConfigMap cache is populated after first lookup
func TestE2E_CachePopulated(t *testing.T) {
	ctx := context.Background()
	ns := "default"
	podName := "e2e-test-cache-" + randomSuffix()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: ns,
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "app",
					Image: "gcr.io/google_containers/pause-amd64:3.1",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    mustParseQuantity("10m"),
							corev1.ResourceMemory: mustParseQuantity("16Mi"),
						},
					},
				},
			},
			RestartPolicy: corev1.RestartPolicyNever,
		},
	}

	_, err := clientset.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Failed to create pod: %v", err)
	}
	defer cleanupPod(t, ctx, ns, podName)

	// Wait for cache ConfigMap to be populated (async write)
	err = wait.PollImmediate(500*time.Millisecond, 10*time.Second, func() (bool, error) {
		cm, err := clientset.CoreV1().ConfigMaps("image-arch-system").Get(ctx, "image-arch-cache", metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		// Check if the image is cached
		for key := range cm.Data {
			if strings.Contains(key, "pause-amd64") {
				return true, nil
			}
		}
		return false, nil
	})

	if err != nil {
		t.Error("Cache ConfigMap was not populated within timeout")
	} else {
		t.Log("✅ Cache ConfigMap populated with image arch data")
	}
}

// TestE2E_WebhookDown_PodStillCreated verifies failurePolicy: Ignore works
func TestE2E_WebhookDown_PodStillCreated(t *testing.T) {
	// This test is informational — to fully test it, you'd scale the webhook to 0
	// and verify pods still get created (without injection)
	t.Skip("Manual test: scale webhook to 0 replicas and verify pods still schedule")
}

// TestE2E_MultiContainer_SingleArchConstraint tests a pod with mixed containers
func TestE2E_MultiContainer_SingleArchConstraint(t *testing.T) {
	ctx := context.Background()
	ns := "default"
	podName := "e2e-test-multicontainer-" + randomSuffix()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: ns,
		},
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{
				{
					Name:  "init",
					Image: "gcr.io/google_containers/pause-amd64:3.1", // single-arch
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    mustParseQuantity("10m"),
							corev1.ResourceMemory: mustParseQuantity("16Mi"),
						},
					},
				},
			},
			Containers: []corev1.Container{
				{
					Name:  "app",
					Image: "registry.k8s.io/pause:3.9", // multi-arch
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    mustParseQuantity("10m"),
							corev1.ResourceMemory: mustParseQuantity("16Mi"),
						},
					},
				},
			},
			RestartPolicy: corev1.RestartPolicyNever,
		},
	}

	created, err := clientset.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Failed to create pod: %v", err)
	}
	defer cleanupPod(t, ctx, ns, podName)

	// Init container is amd64-only → should constrain entire pod
	if created.Spec.NodeSelector == nil {
		t.Fatal("Expected nodeSelector injection due to single-arch init container")
	}
	arch := created.Spec.NodeSelector["kubernetes.io/arch"]
	if arch != "amd64" {
		t.Errorf("Expected amd64, got %q", arch)
	}

	t.Logf("✅ Pod %s: init container arch constraint applied to pod (amd64)", podName)
}

// TestE2E_Deployment_AllReplicasInjected tests via Deployment
func TestE2E_Deployment_AllReplicasInjected(t *testing.T) {
	ctx := context.Background()
	ns := "default"
	deployName := "e2e-test-deploy-" + randomSuffix()

	// Create a deployment with single-arch image
	replicas := int32(3)
	deploy := &appsv1DeploymentSpec(deployName, ns, replicas)

	_, err := clientset.AppsV1().Deployments(ns).Create(ctx, deploy, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Failed to create deployment: %v", err)
	}
	defer clientset.AppsV1().Deployments(ns).Delete(ctx, deployName, metav1.DeleteOptions{})

	// Wait for pods to be created
	time.Sleep(5 * time.Second)

	pods, err := clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("app=%s", deployName),
	})
	if err != nil {
		t.Fatalf("Failed to list pods: %v", err)
	}

	injectedCount := 0
	for _, p := range pods.Items {
		if p.Spec.NodeSelector != nil {
			if p.Spec.NodeSelector["kubernetes.io/arch"] == "amd64" {
				injectedCount++
			}
		}
	}

	if injectedCount != len(pods.Items) {
		t.Errorf("Expected all %d pods to have arch injection, only %d had it",
			len(pods.Items), injectedCount)
	} else {
		t.Logf("✅ Deployment %s: all %d pods have nodeSelector injected", deployName, injectedCount)
	}
}

// --- Helpers ---

func cleanupPod(t *testing.T, ctx context.Context, ns, name string) {
	t.Helper()
	err := clientset.CoreV1().Pods(ns).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil {
		t.Logf("Warning: failed to cleanup pod %s: %v", name, err)
	}
}

func randomSuffix() string {
	return fmt.Sprintf("%d", time.Now().UnixNano()%100000)
}

func mustParseQuantity(s string) resource.Quantity {
	q, _ := resource.ParseQuantity(s)
	return q
}
