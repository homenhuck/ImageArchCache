package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

// CacheEntry represents a cached architecture result
type CacheEntry struct {
	Architectures []string  `json:"architectures"`
	Digest        string    `json:"digest"`
	CachedAt      time.Time `json:"cached_at"`
}

// ArchCache implements a two-level cache: L1 in-memory, L2 ConfigMap (etcd)
type ArchCache struct {
	mu        sync.RWMutex
	local     map[string]*CacheEntry // L1: in-memory
	client    kubernetes.Interface   // L2: ConfigMap
	namespace string
	cmName    string
	ttl       time.Duration
}

// New creates a new ArchCache
func New(client kubernetes.Interface, namespace, cmName string, ttl time.Duration) *ArchCache {
	c := &ArchCache{
		local:     make(map[string]*CacheEntry),
		client:    client,
		namespace: namespace,
		cmName:    cmName,
		ttl:       ttl,
	}

	// Start background TTL cleanup
	go c.cleanupLoop()

	return c
}

// EnsureConfigMap creates the cache ConfigMap if it doesn't exist
func (c *ArchCache) EnsureConfigMap(ctx context.Context) error {
	_, err := c.client.CoreV1().ConfigMaps(c.namespace).Get(ctx, c.cmName, metav1.GetOptions{})
	if err == nil {
		return nil // already exists
	}
	if !errors.IsNotFound(err) {
		return fmt.Errorf("failed to check ConfigMap: %w", err)
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      c.cmName,
			Namespace: c.namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":      "image-arch-webhook",
				"app.kubernetes.io/component": "cache",
			},
		},
		Data: map[string]string{},
	}

	_, err = c.client.CoreV1().ConfigMaps(c.namespace).Create(ctx, cm, metav1.CreateOptions{})
	if err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("failed to create cache ConfigMap: %w", err)
	}

	klog.Infof("Created cache ConfigMap %s/%s", c.namespace, c.cmName)
	return nil
}

// Get retrieves architectures from cache (L1 → L2)
func (c *ArchCache) Get(ctx context.Context, imageRef string) ([]string, bool) {
	key := sanitizeKey(imageRef)

	// L1: in-memory
	c.mu.RLock()
	if entry, ok := c.local[key]; ok {
		c.mu.RUnlock()
		if time.Since(entry.CachedAt) < c.ttl {
			klog.V(3).Infof("Cache L1 hit for %s", imageRef)
			return entry.Architectures, true
		}
		// Expired — fall through
	} else {
		c.mu.RUnlock()
	}

	// L2: ConfigMap (etcd)
	cm, err := c.client.CoreV1().ConfigMaps(c.namespace).Get(ctx, c.cmName, metav1.GetOptions{})
	if err != nil {
		klog.V(2).Infof("Cache L2 read failed: %v", err)
		return nil, false
	}

	if raw, ok := cm.Data[key]; ok {
		var entry CacheEntry
		if err := json.Unmarshal([]byte(raw), &entry); err == nil {
			if time.Since(entry.CachedAt) < c.ttl {
				// Warm L1
				c.mu.Lock()
				c.local[key] = &entry
				c.mu.Unlock()
				klog.V(3).Infof("Cache L2 hit for %s", imageRef)
				return entry.Architectures, true
			}
		}
	}

	return nil, false
}

// Set stores architectures in both cache levels
func (c *ArchCache) Set(ctx context.Context, imageRef string, archs []string, digest string) {
	key := sanitizeKey(imageRef)
	entry := &CacheEntry{
		Architectures: archs,
		Digest:        digest,
		CachedAt:      time.Now(),
	}

	// L1: in-memory
	c.mu.Lock()
	c.local[key] = entry
	c.mu.Unlock()

	// L2: ConfigMap (etcd) — async patch
	go func() {
		raw, _ := json.Marshal(entry)
		patch := fmt.Sprintf(`{"data":{"%s":%s}}`, key, string(mustMarshal(string(raw))))
		_, err := c.client.CoreV1().ConfigMaps(c.namespace).Patch(
			ctx, c.cmName,
			types.MergePatchType,
			[]byte(patch),
			metav1.PatchOptions{},
		)
		if err != nil {
			klog.Warningf("Failed to write cache L2 for %s: %v", imageRef, err)
		} else {
			klog.V(3).Infof("Cache L2 stored for %s: %v", imageRef, archs)
		}
	}()
}

// sanitizeKey converts an image reference into a valid ConfigMap data key
// ConfigMap keys must match [-._a-zA-Z0-9]+
func sanitizeKey(imageRef string) string {
	r := strings.NewReplacer(
		"/", "_",
		":", "__",
		"@", "___",
	)
	return r.Replace(imageRef)
}

// mustMarshal is a helper to JSON-encode a string value for the patch
func mustMarshal(s string) []byte {
	b, _ := json.Marshal(s)
	return b
}

// cleanupLoop periodically removes expired entries from L1 cache
func (c *ArchCache) cleanupLoop() {
	ticker := time.NewTicker(30 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		c.mu.Lock()
		now := time.Now()
		for key, entry := range c.local {
			if now.Sub(entry.CachedAt) > c.ttl {
				delete(c.local, key)
			}
		}
		c.mu.Unlock()
		klog.V(3).Info("Cache L1 cleanup completed")
	}
}
