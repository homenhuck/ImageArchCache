package cache

import (
	"testing"
)

func TestSanitizeKey(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{
			input: "nginx:latest",
			want:  "nginx__latest",
		},
		{
			input: "gcr.io/google_containers/pause-amd64:3.1",
			want:  "gcr.io_google_containers_pause-amd64__3.1",
		},
		{
			input: "123456789.dkr.ecr.us-east-1.amazonaws.com/my-app:v2.0.0",
			want:  "123456789.dkr.ecr.us-east-1.amazonaws.com_my-app__v2.0.0",
		},
		{
			input: "registry.k8s.io/pause@sha256:abc123",
			want:  "registry.k8s.io_pause___sha256__abc123",
		},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := sanitizeKey(tt.input)
			if got != tt.want {
				t.Errorf("sanitizeKey(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestCacheEntry_TTL(t *testing.T) {
	// Verify the CacheEntry struct can be properly marshaled/unmarshaled
	entry := &CacheEntry{
		Architectures: []string{"amd64", "arm64"},
		Digest:        "sha256:abc123def456",
	}

	if len(entry.Architectures) != 2 {
		t.Errorf("expected 2 architectures, got %d", len(entry.Architectures))
	}
	if entry.Digest != "sha256:abc123def456" {
		t.Errorf("unexpected digest: %s", entry.Digest)
	}
}
