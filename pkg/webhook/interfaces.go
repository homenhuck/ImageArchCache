package webhook

import "context"

// CacheInterface abstracts the cache for testability
type CacheInterface interface {
	Get(ctx context.Context, imageRef string) ([]string, bool)
	Set(ctx context.Context, imageRef string, archs []string, digest string)
}

// RegistryInterface abstracts the registry client for testability
type RegistryInterface interface {
	GetArchitectures(ctx context.Context, imageRef string) (*ArchResult, error)
}

// ArchResult is a simplified result for the interface
type ArchResult struct {
	Image         string
	Digest        string
	Architectures []string
}
