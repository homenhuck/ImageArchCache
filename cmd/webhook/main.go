package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"

	"github.com/image-arch-webhook/pkg/cache"
	"github.com/image-arch-webhook/pkg/registry"
	"github.com/image-arch-webhook/pkg/webhook"
)

func main() {
	var (
		tlsCert    string
		tlsKey     string
		port       int
		cacheTTL   time.Duration
		cacheNs    string
		cacheCM    string
		metricsPort int
	)

	flag.StringVar(&tlsCert, "tls-cert", "/etc/webhook/certs/tls.crt", "TLS certificate path")
	flag.StringVar(&tlsKey, "tls-key", "/etc/webhook/certs/tls.key", "TLS key path")
	flag.IntVar(&port, "port", 8443, "Webhook server port")
	flag.DurationVar(&cacheTTL, "cache-ttl", 24*time.Hour, "Cache TTL for resolved architectures")
	flag.StringVar(&cacheNs, "cache-namespace", "image-arch-system", "Namespace for cache ConfigMap")
	flag.StringVar(&cacheCM, "cache-configmap", "image-arch-cache", "ConfigMap name for cache")
	flag.IntVar(&metricsPort, "metrics-port", 8080, "Metrics/health port")
	flag.Parse()

	klog.InitFlags(nil)

	// Build in-cluster client
	config, err := rest.InClusterConfig()
	if err != nil {
		klog.Fatalf("Failed to get in-cluster config: %v", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		klog.Fatalf("Failed to create kubernetes client: %v", err)
	}

	// Initialize components
	archCache := cache.New(clientset, cacheNs, cacheCM, cacheTTL)
	registryClient := registry.NewClient()
	handler := webhook.NewHandler(archCache, registryClient)

	// Ensure cache ConfigMap exists
	ctx := context.Background()
	if err := archCache.EnsureConfigMap(ctx); err != nil {
		klog.Fatalf("Failed to ensure cache ConfigMap: %v", err)
	}

	// TLS config
	cert, err := tls.LoadX509KeyPair(tlsCert, tlsKey)
	if err != nil {
		klog.Fatalf("Failed to load TLS cert: %v", err)
	}

	// Webhook server
	mux := http.NewServeMux()
	mux.HandleFunc("/mutate", handler.ServeMutate)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
	})

	server := &http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: mux,
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{cert},
		},
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	// Metrics/health server (non-TLS for probes)
	healthMux := http.NewServeMux()
	healthMux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	healthMux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	healthServer := &http.Server{
		Addr:    fmt.Sprintf(":%d", metricsPort),
		Handler: healthMux,
	}

	// Start servers
	go func() {
		klog.Infof("Starting health server on :%d", metricsPort)
		if err := healthServer.ListenAndServe(); err != http.ErrServerClosed {
			klog.Fatalf("Health server failed: %v", err)
		}
	}()

	go func() {
		klog.Infof("Starting webhook server on :%d", port)
		if err := server.ListenAndServeTLS("", ""); err != http.ErrServerClosed {
			klog.Fatalf("Webhook server failed: %v", err)
		}
	}()

	// Graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	<-sigCh

	klog.Info("Shutting down...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	server.Shutdown(shutdownCtx)
	healthServer.Shutdown(shutdownCtx)
	klog.Info("Shutdown complete")
}
