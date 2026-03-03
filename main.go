package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var (
	configPath     = flag.String("config", "", "Path to YAML or JSON config file")
	port           = flag.String("port", "12345", "Port to listen on")
	cacheDir       = flag.String("cache", "./cache", "Cache directory path")
	upstream       = flag.String("upstream", "https://proxy.golang.org", "Upstream proxy URL(s), comma-separated for fallback")
	httpProxy      = flag.String("proxy", "", "HTTP/HTTPS/SOCKS5 proxy URL")
	dnsServer      = flag.String("dns", "", "DNS server URL")
	writeTimeout   = flag.String("write-timeout", "30m", "HTTP write timeout")
	maxCacheAge    = flag.String("max-cache-age", "0s", "Remove cached modules unused for this duration (0s = unlimited, disables cleanup)")
	cleanupInterval = flag.String("cleanup-interval", "24h", "How often to run cache cleanup")
	gosumdb        = flag.String("gosumdb", "off", "Checksum database URL (`off` to disable; e.g. `sum.golang.org` to enable)")
	gonosumdb      = flag.String("gonosumdb", "", "Comma-separated module patterns to skip checksum verification")
	retryAttempts        = flag.Int("retry-attempts", 3, "Number of retry attempts for upstream requests")
	retryBackoff         = flag.String("retry-backoff", "100ms", "Initial backoff duration for retries")
	downloadConnections  = flag.Int("download-connections", 4, "Parallel connections for zip downloads (1=disabled)")
	maxConcurrentDownloads = flag.Int("max-concurrent-downloads", 4, "Maximum number of packages downloading concurrently (0=unlimited)")
)

func main() {
	flag.Parse()

	cfg := DefaultConfig()

	// Load config file if specified
	if *configPath != "" {
		fileCfg, err := LoadConfig(*configPath)
		if err != nil {
			log.Fatalf("Failed to load config from %s: %v", *configPath, err)
		}
		mergeConfig(&cfg, fileCfg)
	}

	// Environment overrides
	if v := os.Getenv("PORT"); v != "" {
		cfg.Port = v
	}
	if v := os.Getenv("CACHE_DIR"); v != "" {
		cfg.CacheDir = v
	}
	if v := os.Getenv("UPSTREAM_PROXY"); v != "" {
		cfg.Upstreams = parseUpstreams(v)
	}
	if v := os.Getenv("HTTP_PROXY"); v != "" && cfg.Proxy == "" {
		cfg.Proxy = v
	}
	if v := os.Getenv("HTTPS_PROXY"); v != "" && cfg.Proxy == "" {
		cfg.Proxy = v
	}
	if v := os.Getenv("SOCKS5_PROXY"); v != "" && cfg.Proxy == "" {
		cfg.Proxy = v
	}
	if v := os.Getenv("DNS_SERVER"); v != "" {
		cfg.DNSServer = v
	}
	if v := os.Getenv("WRITE_TIMEOUT"); v != "" {
		cfg.WriteTimeout = v
	}
	if v := os.Getenv("MAX_CACHE_AGE"); v != "" {
		cfg.MaxCacheAge = v
	}
	if v := os.Getenv("CLEANUP_INTERVAL"); v != "" {
		cfg.CleanupInterval = v
	}
	if v := os.Getenv("GOSUMDB"); v != "" {
		cfg.GOSUMDB = v
	}
	if v := os.Getenv("GONOSUMDB"); v != "" {
		cfg.GONOSUMDB = v
	}
	if v := os.Getenv("DOWNLOAD_CONNECTIONS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.DownloadConnections = n
		}
	}
	if v := os.Getenv("MAX_CONCURRENT_DOWNLOADS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			cfg.MaxConcurrentDownloads = n
		}
	}

	// Flag overrides (highest priority)
	cfg.Port = *port
	cfg.CacheDir = *cacheDir
	cfg.Upstreams = parseUpstreams(*upstream)
	if *httpProxy != "" {
		cfg.Proxy = *httpProxy
	}
	cfg.DNSServer = *dnsServer
	cfg.WriteTimeout = *writeTimeout
	cfg.MaxCacheAge = *maxCacheAge
	cfg.CleanupInterval = *cleanupInterval
	cfg.GOSUMDB = *gosumdb
	cfg.GONOSUMDB = *gonosumdb
	cfg.RetryAttempts = *retryAttempts
	cfg.RetryBackoff = *retryBackoff
	cfg.DownloadConnections = *downloadConnections
	cfg.MaxConcurrentDownloads = *maxConcurrentDownloads

	// Resolve proxy from env if flag not set
	if cfg.Proxy == "" {
		if v := os.Getenv("HTTP_PROXY"); v != "" {
			cfg.Proxy = v
		} else if v := os.Getenv("HTTPS_PROXY"); v != "" {
			cfg.Proxy = v
		} else if v := os.Getenv("SOCKS5_PROXY"); v != "" {
			cfg.Proxy = v
		}
	}

	// Parse durations
	writeTimeoutDur, err := time.ParseDuration(cfg.WriteTimeout)
	if err != nil {
		log.Fatalf("Invalid write-timeout value '%s': %v", cfg.WriteTimeout, err)
	}
	var maxCacheAgeDur time.Duration
	if cfg.MaxCacheAge != "" {
		maxCacheAgeDur, err = time.ParseDuration(cfg.MaxCacheAge)
		if err != nil {
			log.Fatalf("Invalid max-cache-age value '%s': %v", cfg.MaxCacheAge, err)
		}
	}
	cleanupIntervalDur, err := time.ParseDuration(cfg.CleanupInterval)
	if err != nil {
		log.Fatalf("Invalid cleanup-interval value '%s': %v", cfg.CleanupInterval, err)
	}
	retryBackoffDur, err := time.ParseDuration(cfg.RetryBackoff)
	if err != nil {
		log.Fatalf("Invalid retry-backoff value '%s': %v", cfg.RetryBackoff, err)
	}

	if err := os.MkdirAll(cfg.CacheDir, 0755); err != nil {
		log.Fatalf("Failed to create cache directory: %v", err)
	}

	proxy := NewProxyFromConfig(&cfg, maxCacheAgeDur, cleanupIntervalDur, retryBackoffDur)

	mux := http.NewServeMux()
	mux.HandleFunc("/", proxy.HandleRequest)

	addr := fmt.Sprintf(":%s", cfg.Port)
	srv := &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: writeTimeoutDur,
		IdleTimeout:  15 * time.Second,
	}

	log.Printf("Starting Go module proxy server")
	log.Printf("  Port: %s", cfg.Port)
	log.Printf("  Cache directory: %s", cfg.CacheDir)
	log.Printf("  Upstreams: %v", cfg.Upstreams)
	if cfg.Proxy != "" {
		log.Printf("  HTTP/SOCKS5 proxy: %s", cfg.Proxy)
	}
	if cfg.DNSServer != "" {
		log.Printf("  DNS server: %s", cfg.DNSServer)
	}
	log.Printf("  Write timeout: %v", writeTimeoutDur)
	if maxCacheAgeDur <= 0 {
		log.Printf("  Max cache age: unlimited (cleanup disabled)")
	} else {
		log.Printf("  Max cache age: %v (cleanup every %v)", maxCacheAgeDur, cleanupIntervalDur)
	}
	log.Printf("  Set GOPROXY=http://localhost%s,direct", addr)

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server failed: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("Shutting down server...")
	proxy.Shutdown()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		log.Fatalf("Server forced to shutdown: %v", err)
	}

	log.Println("Server exited")
}

func parseUpstreams(s string) []string {
	var out []string
	for _, u := range strings.Split(s, ",") {
		u = strings.TrimSpace(u)
		if u != "" {
			u = strings.TrimSuffix(u, "/")
			out = append(out, u)
		}
	}
	if len(out) == 0 {
		return []string{"https://proxy.golang.org"}
	}
	return out
}

func mergeConfig(dst *Config, src *Config) {
	if src.Port != "" {
		dst.Port = src.Port
	}
	if src.CacheDir != "" {
		dst.CacheDir = src.CacheDir
	}
	if len(src.Upstreams) > 0 {
		dst.Upstreams = src.Upstreams
	}
	if len(src.PrivateUpstreams) > 0 {
		dst.PrivateUpstreams = src.PrivateUpstreams
	}
	if src.Proxy != "" {
		dst.Proxy = src.Proxy
	}
	if src.DNSServer != "" {
		dst.DNSServer = src.DNSServer
	}
	if src.WriteTimeout != "" {
		dst.WriteTimeout = src.WriteTimeout
	}
	if src.MaxCacheAge != "" {
		dst.MaxCacheAge = src.MaxCacheAge
	}
	if src.CleanupInterval != "" {
		dst.CleanupInterval = src.CleanupInterval
	}
	if src.GOSUMDB != "" {
		dst.GOSUMDB = src.GOSUMDB
	}
	if src.GONOSUMDB != "" {
		dst.GONOSUMDB = src.GONOSUMDB
	}
	if src.RetryAttempts > 0 {
		dst.RetryAttempts = src.RetryAttempts
	}
	if src.RetryBackoff != "" {
		dst.RetryBackoff = src.RetryBackoff
	}
	if src.DownloadConnections > 0 {
		dst.DownloadConnections = src.DownloadConnections
	}
	if src.MaxConcurrentDownloads >= 0 {
		dst.MaxConcurrentDownloads = src.MaxConcurrentDownloads
	}
}
