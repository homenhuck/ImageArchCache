package registry

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"k8s.io/klog/v2"
)

// Client handles registry manifest lookups
type Client struct {
	keychain authn.Keychain
}

// NewClient creates a registry client with default keychain
// (supports ECR, GCR, DockerHub, etc. via credential helpers)
func NewClient() *Client {
	return &Client{
		keychain: authn.DefaultKeychain,
	}
}

// ImageArchResult holds the resolved architectures for an image
type ImageArchResult struct {
	Image          string   `json:"image"`
	Digest         string   `json:"digest"`
	Architectures  []string `json:"architectures"`
}

// GetArchitectures queries the registry and returns supported architectures
func (c *Client) GetArchitectures(ctx context.Context, imageRef string) (*ImageArchResult, error) {
	ref, err := name.ParseReference(imageRef)
	if err != nil {
		return nil, fmt.Errorf("failed to parse image reference %q: %w", imageRef, err)
	}

	// Try to get as an image index (multi-arch manifest list)
	desc, err := remote.Get(ref, remote.WithAuthFromKeychain(c.keychain), remote.WithContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("failed to fetch manifest for %q: %w", imageRef, err)
	}

	result := &ImageArchResult{
		Image:  imageRef,
		Digest: desc.Digest.String(),
	}

	// Check if it's a manifest list (OCI index or Docker manifest list)
	switch desc.MediaType {
	case "application/vnd.oci.image.index.v1+json",
		"application/vnd.docker.distribution.manifest.list.v2+json":
		// Multi-arch manifest list
		index, err := desc.ImageIndex()
		if err != nil {
			return nil, fmt.Errorf("failed to parse image index for %q: %w", imageRef, err)
		}

		indexManifest, err := index.IndexManifest()
		if err != nil {
			return nil, fmt.Errorf("failed to get index manifest for %q: %w", imageRef, err)
		}

		archSet := make(map[string]struct{})
		for _, m := range indexManifest.Manifests {
			if m.Platform != nil && m.Platform.OS == "linux" {
				archSet[m.Platform.Architecture] = struct{}{}
			}
		}

		for arch := range archSet {
			result.Architectures = append(result.Architectures, arch)
		}

	case "application/vnd.oci.image.manifest.v1+json",
		"application/vnd.docker.distribution.manifest.v2+json":
		// Single manifest — need to get the config to find the architecture
		img, err := desc.Image()
		if err != nil {
			return nil, fmt.Errorf("failed to get image for %q: %w", imageRef, err)
		}

		config, err := img.ConfigFile()
		if err != nil {
			return nil, fmt.Errorf("failed to get config for %q: %w", imageRef, err)
		}

		result.Architectures = []string{config.Architecture}

	default:
		return nil, fmt.Errorf("unknown media type %q for %q", desc.MediaType, imageRef)
	}

	if len(result.Architectures) == 0 {
		return nil, fmt.Errorf("no linux architectures found for %q", imageRef)
	}

	klog.V(2).Infof("Resolved architectures for %s: %s", imageRef, strings.Join(result.Architectures, ","))
	return result, nil
}
