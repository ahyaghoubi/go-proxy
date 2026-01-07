package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

var (
	port      = flag.String("port", "12345", "Port to listen on")
	cacheDir  = flag.String("cache", "./cache", "Cache directory path")
	upstream  = flag.String("upstream", "https://proxy.golang.org", "Upstream proxy URL")
	httpProxy = flag.String("proxy", "", "HTTP/HTTPS/SOCKS5 proxy URL (e.g., http://proxy:8080 or socks5://proxy:1080)")
	dnsServer = flag.String("dns", "", "DNS server URL (e.g., 8.8.8.8:53, https://cloudflare-dns.com/dns-query, tls://1.1.1.1:853)")
)

func main() {
	flag.Parse()

	// Support environment variables (env vars override flags)
	if envPort := os.Getenv("PORT"); envPort != "" {
		*port = envPort
	}
	if envCache := os.Getenv("CACHE_DIR"); envCache != "" {
		*cacheDir = envCache
	}
	if envUpstream := os.Getenv("UPSTREAM_PROXY"); envUpstream != "" {
		*upstream = envUpstream
	}
	// Proxy from environment (only if flag not set)
	if *httpProxy == "" {
		if envProxy := os.Getenv("HTTP_PROXY"); envProxy != "" {
			*httpProxy = envProxy
		} else if envProxy := os.Getenv("HTTPS_PROXY"); envProxy != "" {
			*httpProxy = envProxy
		} else if envProxy := os.Getenv("SOCKS5_PROXY"); envProxy != "" {
			*httpProxy = envProxy
		}
	}
	// DNS from environment (only if flag not set)
	if *dnsServer == "" {
		if envDNS := os.Getenv("DNS_SERVER"); envDNS != "" {
			*dnsServer = envDNS
		}
	}

	// Ensure cache directory exists
	if err := os.MkdirAll(*cacheDir, 0755); err != nil {
		log.Fatalf("Failed to create cache directory: %v", err)
	}

	// Create proxy handler
	proxy := NewProxy(*cacheDir, *upstream, *httpProxy, *dnsServer)

	// Setup HTTP server
	mux := http.NewServeMux()
	mux.HandleFunc("/", proxy.HandleRequest)

	addr := fmt.Sprintf(":%s", *port)
	srv := &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 10 * time.Minute, // Increased for large zip files
		IdleTimeout:  60 * time.Second,
	}

	// Log startup configuration
	log.Printf("Starting Go module proxy server")
	log.Printf("  Port: %s", *port)
	log.Printf("  Cache directory: %s", *cacheDir)
	log.Printf("  Upstream proxy: %s", *upstream)
	if *httpProxy != "" {
		log.Printf("  HTTP/SOCKS5 proxy: %s", *httpProxy)
	}
	if *dnsServer != "" {
		log.Printf("  DNS server: %s", *dnsServer)
	}
	log.Printf("  Set GOPROXY=http://localhost%s,direct", addr)

	// Start server in a goroutine
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server failed: %v", err)
		}
	}()

	// Wait for interrupt signal for graceful shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("Shutting down server...")

	// Create context with timeout for graceful shutdown
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Shutdown server gracefully
	if err := srv.Shutdown(ctx); err != nil {
		log.Fatalf("Server forced to shutdown: %v", err)
	}

	log.Println("Server exited")
}
