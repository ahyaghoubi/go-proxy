package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
	"golang.org/x/net/proxy"
)

// DNSResolver handles different DNS protocol types
type DNSResolver interface {
	LookupIP(ctx context.Context, host string) ([]net.IP, error)
}

// StandardDNSResolver uses UDP DNS
type StandardDNSResolver struct {
	server string
}

func (r *StandardDNSResolver) LookupIP(ctx context.Context, host string) ([]net.IP, error) {
	resolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			d := net.Dialer{Timeout: 5 * time.Second}
			return d.DialContext(ctx, "udp", r.server)
		},
	}
	addrs, err := resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	ips := make([]net.IP, len(addrs))
	for i, addr := range addrs {
		ips[i] = addr.IP
	}
	return ips, nil
}

// DoHResolver uses DNS-over-HTTPS
type DoHResolver struct {
	client   *http.Client
	endpoint string
}

func (r *DoHResolver) LookupIP(ctx context.Context, host string) ([]net.IP, error) {
	// Simple DoH implementation using JSON format
	dohURL := fmt.Sprintf("%s?name=%s&type=A", r.endpoint, url.QueryEscape(host))
	req, err := http.NewRequestWithContext(ctx, "GET", dohURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/dns-json")

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("DoH server returned status %d", resp.StatusCode)
	}

	var dohResponse struct {
		Answer []struct {
			Type int    `json:"type"`
			Data string `json:"data"`
		} `json:"Answer"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&dohResponse); err != nil {
		return nil, err
	}

	var ips []net.IP
	for _, answer := range dohResponse.Answer {
		if answer.Type == 1 { // A record
			if ip := net.ParseIP(answer.Data); ip != nil {
				ips = append(ips, ip)
			}
		}
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("no A records found for %s", host)
	}
	return ips, nil
}

// DoTResolver uses DNS-over-TLS
type DoTResolver struct {
	server string
	client *dns.Client
}

func (r *DoTResolver) LookupIP(ctx context.Context, host string) ([]net.IP, error) {
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(host), dns.TypeA)

	// Create TLS connection
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", r.server, &tls.Config{
		ServerName: strings.Split(r.server, ":")[0],
	})
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	dnsConn := &dns.Conn{Conn: conn}
	defer dnsConn.Close()

	// Set deadline from context
	if deadline, ok := ctx.Deadline(); ok {
		dnsConn.SetDeadline(deadline)
	}

	err = dnsConn.WriteMsg(m)
	if err != nil {
		return nil, err
	}

	reply, err := dnsConn.ReadMsg()
	if err != nil {
		return nil, err
	}

	var ips []net.IP
	for _, rr := range reply.Answer {
		if a, ok := rr.(*dns.A); ok {
			ips = append(ips, a.A)
		}
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("no A records found for %s", host)
	}
	return ips, nil
}

// DoQResolver uses DNS-over-QUIC
type DoQResolver struct {
	server string
	client *dns.Client
}

func (r *DoQResolver) LookupIP(ctx context.Context, host string) ([]net.IP, error) {
	// DoQ implementation using miekg/dns
	// Note: Full DoQ support requires QUIC library
	// For now, fallback to DoT
	dotResolver := &DoTResolver{
		server: r.server,
		client: r.client,
	}
	return dotResolver.LookupIP(ctx, host)
}

// createDNSResolver creates appropriate DNS resolver based on URL
func createDNSResolver(dnsURL string) (DNSResolver, error) {
	if dnsURL == "" {
		return nil, nil
	}

	// Check if it's a DoH URL
	if strings.HasPrefix(dnsURL, "https://") {
		return &DoHResolver{
			client: &http.Client{
				Timeout: 10 * time.Second,
			},
			endpoint: dnsURL,
		}, nil
	}

	// Check if it's DoQ (quic://)
	if strings.HasPrefix(dnsURL, "quic://") {
		server := strings.TrimPrefix(dnsURL, "quic://")
		if !strings.Contains(server, ":") {
			server += ":853"
		}
		return &DoQResolver{
			server: server,
			client: &dns.Client{Net: "tcp-tls"},
		}, nil
	}

	// Check if it's DoT (tls://)
	if strings.HasPrefix(dnsURL, "tls://") {
		server := strings.TrimPrefix(dnsURL, "tls://")
		if !strings.Contains(server, ":") {
			server += ":853"
		}
		return &DoTResolver{
			server: server,
			client: &dns.Client{Net: "tcp-tls"},
		}, nil
	}

	// Standard DNS (udp:// or plain IP:port)
	server := strings.TrimPrefix(dnsURL, "udp://")
	if !strings.Contains(server, ":") {
		server += ":53"
	}
	return &StandardDNSResolver{server: server}, nil
}

// createDialer creates a custom dialer with DNS resolver support
func createDialer(dnsResolver DNSResolver) func(ctx context.Context, network, address string) (net.Conn, error) {
	dialer := &net.Dialer{
		Timeout:   5 * time.Second,
		KeepAlive: 30 * time.Second,
	}

	if dnsResolver != nil {
		dialer.Resolver = &net.Resolver{
			PreferGo: true,
			Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
				host, port, err := net.SplitHostPort(address)
				if err != nil {
					return nil, err
				}
				ips, err := dnsResolver.LookupIP(ctx, host)
				if err != nil || len(ips) == 0 {
					return nil, fmt.Errorf("failed to resolve %s: %v", host, err)
				}
				// Use first IP
				resolvedAddr := net.JoinHostPort(ips[0].String(), port)
				d := net.Dialer{Timeout: 5 * time.Second}
				return d.DialContext(ctx, network, resolvedAddr)
			},
		}
	}

	return dialer.DialContext
}

// Proxy handles Go module proxy requests with disk caching
type Proxy struct {
	cacheDir string
	upstream string
	client   *http.Client
	mu       sync.RWMutex
}

// NewProxy creates a new proxy instance with configured HTTP client
func NewProxy(cacheDir, upstream, httpProxy, dnsServer string) *Proxy {
	// Create DNS resolver
	dnsResolver, err := createDNSResolver(dnsServer)
	if err != nil {
		log.Printf("[WARN] Failed to create DNS resolver: %v", err)
		dnsResolver = nil
	} else if dnsResolver != nil {
		log.Printf("Using DNS resolver: %s", dnsServer)
	}

	// Create dialer with DNS support
	dialer := createDialer(dnsResolver)

	// Configure HTTP client with proper timeouts and connection pooling
	transport := &http.Transport{
		DialContext:           dialer,
		TLSHandshakeTimeout:   5 * time.Second,  // TLS handshake timeout
		ResponseHeaderTimeout: 10 * time.Second, // Response header timeout
		IdleConnTimeout:       90 * time.Second, // Idle connection timeout
		MaxIdleConns:          100,              // Maximum idle connections
		MaxIdleConnsPerHost:   10,               // Maximum idle connections per host
	}

	// Determine proxy URL: flag > HTTP_PROXY > HTTPS_PROXY > SOCKS5_PROXY
	proxyURL := httpProxy
	if proxyURL == "" {
		proxyURL = os.Getenv("HTTP_PROXY")
	}
	if proxyURL == "" {
		proxyURL = os.Getenv("HTTPS_PROXY")
	}
	if proxyURL == "" {
		proxyURL = os.Getenv("SOCKS5_PROXY")
	}

	// Configure proxy if provided
	if proxyURL != "" {
		parsedURL, err := url.Parse(proxyURL)
		if err != nil {
			log.Printf("[WARN] Invalid proxy URL '%s': %v", proxyURL, err)
		} else {
			switch parsedURL.Scheme {
			case "http", "https":
				// HTTP/HTTPS proxy
				transport.Proxy = http.ProxyURL(parsedURL)
				log.Printf("Using HTTP proxy: %s", proxyURL)
			case "socks5", "socks5h":
				// SOCKS5 proxy
				socksDialer, err := proxy.SOCKS5("tcp", parsedURL.Host, nil, proxy.Direct)
				if err != nil {
					log.Printf("[WARN] Failed to create SOCKS5 dialer: %v", err)
				} else {
					// For SOCKS5, we still want DNS resolution to use custom DNS if specified
					if dnsResolver != nil {
						// Create a wrapper that uses custom DNS before SOCKS5
						transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
							// Resolve address using custom DNS
							host, port, err := net.SplitHostPort(address)
							if err != nil {
								return nil, err
							}
							ips, err := dnsResolver.LookupIP(ctx, host)
							if err != nil || len(ips) == 0 {
								return nil, fmt.Errorf("failed to resolve %s: %v", host, err)
							}
							// Use first IP
							resolvedAddr := net.JoinHostPort(ips[0].String(), port)
							return socksDialer.(proxy.ContextDialer).DialContext(ctx, network, resolvedAddr)
						}
					} else {
						transport.DialContext = socksDialer.(proxy.ContextDialer).DialContext
					}
					log.Printf("Using SOCKS5 proxy: %s", proxyURL)
				}
			default:
				log.Printf("[WARN] Unsupported proxy scheme: %s (supported: http, https, socks5, socks5h)", parsedURL.Scheme)
			}
		}
	}

	return &Proxy{
		cacheDir: cacheDir,
		upstream: strings.TrimSuffix(upstream, "/"),
		client: &http.Client{
			Timeout:   5 * time.Minute, // Increased timeout for large files (zip downloads)
			Transport: transport,
		},
		mu: sync.RWMutex{},
	}
}

// HandleRequest routes requests to appropriate handlers
func (p *Proxy) HandleRequest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/")

	// Handle health check endpoint
	if path == "health" || path == "healthz" {
		p.handleHealth(w, r)
		return
	}

	log.Printf("[%s] %s %s", r.RemoteAddr, r.Method, path)

	// Route to appropriate handler based on path
	if strings.HasSuffix(path, "/@v/list") {
		p.handleList(w, r, path)
	} else if strings.HasSuffix(path, ".info") {
		p.handleInfo(w, r, path)
	} else if strings.HasSuffix(path, ".mod") {
		p.handleMod(w, r, path)
	} else if strings.HasSuffix(path, ".zip") {
		p.handleZip(w, r, path)
	} else {
		http.Error(w, "Not found", http.StatusNotFound)
	}
}

// handleHealth handles health check requests
func (p *Proxy) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"ok"}`))
}

// handleList handles GET /<module>/@v/list requests
func (p *Proxy) handleList(w http.ResponseWriter, r *http.Request, path string) {
	cachePath := cachePath(p.cacheDir, path)

	// Try cache first (read lock)
	p.mu.RLock()
	cached, err := readCache(cachePath)
	p.mu.RUnlock()

	if err == nil {
		log.Printf("[CACHE HIT] %s", path)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write(cached)
		return
	}

	log.Printf("[CACHE MISS] %s", path)

	// Fetch from upstream
	url := fmt.Sprintf("%s/%s", p.upstream, path)
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, url, nil)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to create request: %v", err), http.StatusInternalServerError)
		return
	}

	resp, err := p.client.Do(req)
	if err != nil {
		log.Printf("[ERROR] Failed to fetch %s: %v", url, err)
		http.Error(w, fmt.Sprintf("Failed to fetch: %v", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("[ERROR] Upstream returned %d for %s", resp.StatusCode, url)
		http.Error(w, fmt.Sprintf("Upstream error: %d", resp.StatusCode), resp.StatusCode)
		return
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("[ERROR] Failed to read response for %s: %v", url, err)
		http.Error(w, fmt.Sprintf("Failed to read response: %v", err), http.StatusInternalServerError)
		return
	}

	// Cache the response (write lock)
	p.mu.Lock()
	if err := writeCache(cachePath, data); err != nil {
		log.Printf("[WARN] Failed to cache %s: %v", path, err)
	}
	p.mu.Unlock()

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write(data)
}

// handleInfo handles GET /<module>/@v/<version>.info requests
func (p *Proxy) handleInfo(w http.ResponseWriter, r *http.Request, path string) {
	cachePath := cachePath(p.cacheDir, path)

	// Try cache first (read lock)
	p.mu.RLock()
	cached, err := readCache(cachePath)
	p.mu.RUnlock()

	if err == nil {
		log.Printf("[CACHE HIT] %s", path)
		w.Header().Set("Content-Type", "application/json")
		w.Write(cached)
		return
	}

	log.Printf("[CACHE MISS] %s", path)

	// Fetch from upstream
	url := fmt.Sprintf("%s/%s", p.upstream, path)
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, url, nil)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to create request: %v", err), http.StatusInternalServerError)
		return
	}

	resp, err := p.client.Do(req)
	if err != nil {
		log.Printf("[ERROR] Failed to fetch %s: %v", url, err)
		http.Error(w, fmt.Sprintf("Failed to fetch: %v", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("[ERROR] Upstream returned %d for %s", resp.StatusCode, url)
		http.Error(w, fmt.Sprintf("Upstream error: %d", resp.StatusCode), resp.StatusCode)
		return
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("[ERROR] Failed to read response for %s: %v", url, err)
		http.Error(w, fmt.Sprintf("Failed to read response: %v", err), http.StatusInternalServerError)
		return
	}

	// Validate JSON
	var info map[string]interface{}
	if err := json.Unmarshal(data, &info); err != nil {
		log.Printf("[ERROR] Invalid JSON from upstream for %s: %v", url, err)
		http.Error(w, fmt.Sprintf("Invalid JSON: %v", err), http.StatusBadGateway)
		return
	}

	// Cache the response (write lock)
	p.mu.Lock()
	if err := writeCache(cachePath, data); err != nil {
		log.Printf("[WARN] Failed to cache %s: %v", path, err)
	}
	p.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

// handleMod handles GET /<module>/@v/<version>.mod requests
func (p *Proxy) handleMod(w http.ResponseWriter, r *http.Request, path string) {
	cachePath := cachePath(p.cacheDir, path)

	// Try cache first (read lock)
	p.mu.RLock()
	cached, err := readCache(cachePath)
	p.mu.RUnlock()

	if err == nil {
		log.Printf("[CACHE HIT] %s", path)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write(cached)
		return
	}

	log.Printf("[CACHE MISS] %s", path)

	// Fetch from upstream
	url := fmt.Sprintf("%s/%s", p.upstream, path)
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, url, nil)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to create request: %v", err), http.StatusInternalServerError)
		return
	}

	resp, err := p.client.Do(req)
	if err != nil {
		log.Printf("[ERROR] Failed to fetch %s: %v", url, err)
		http.Error(w, fmt.Sprintf("Failed to fetch: %v", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("[ERROR] Upstream returned %d for %s", resp.StatusCode, url)
		http.Error(w, fmt.Sprintf("Upstream error: %d", resp.StatusCode), resp.StatusCode)
		return
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("[ERROR] Failed to read response for %s: %v", url, err)
		http.Error(w, fmt.Sprintf("Failed to read response: %v", err), http.StatusInternalServerError)
		return
	}

	// Cache the response (write lock)
	p.mu.Lock()
	if err := writeCache(cachePath, data); err != nil {
		log.Printf("[WARN] Failed to cache %s: %v", path, err)
	}
	p.mu.Unlock()

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write(data)
}

// handleZip handles GET /<module>/@v/<version>.zip requests
func (p *Proxy) handleZip(w http.ResponseWriter, r *http.Request, path string) {
	cachePath := cachePath(p.cacheDir, path)

	// Try cache first (read lock)
	p.mu.RLock()
	file, err := os.Open(cachePath)
	p.mu.RUnlock()

	if err == nil {
		defer file.Close()
		stat, err := file.Stat()
		if err == nil {
			log.Printf("[CACHE HIT] %s", path)
			w.Header().Set("Content-Type", "application/zip")
			w.Header().Set("Content-Length", fmt.Sprintf("%d", stat.Size()))
			io.Copy(w, file)
			return
		}
		file.Close()
	}

	log.Printf("[CACHE MISS] %s", path)

	// Fetch from upstream
	// Use extended context timeout for zip files (up to 10 minutes)
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()

	url := fmt.Sprintf("%s/%s", p.upstream, path)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to create request: %v", err), http.StatusInternalServerError)
		return
	}

	resp, err := p.client.Do(req)
	if err != nil {
		log.Printf("[ERROR] Failed to fetch %s: %v", url, err)
		http.Error(w, fmt.Sprintf("Failed to fetch: %v", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("[ERROR] Upstream returned %d for %s", resp.StatusCode, url)
		http.Error(w, fmt.Sprintf("Upstream error: %d", resp.StatusCode), resp.StatusCode)
		return
	}

	// Create cache directory for this file
	if err := os.MkdirAll(filepath.Dir(cachePath), 0755); err != nil {
		log.Printf("[ERROR] Failed to create cache dir for %s: %v", path, err)
		http.Error(w, fmt.Sprintf("Failed to create cache dir: %v", err), http.StatusInternalServerError)
		return
	}

	// Write to cache and response simultaneously
	cacheFile, err := os.Create(cachePath + ".tmp")
	if err != nil {
		log.Printf("[ERROR] Failed to create cache file for %s: %v", path, err)
		http.Error(w, fmt.Sprintf("Failed to create cache file: %v", err), http.StatusInternalServerError)
		return
	}

	// Set headers before writing
	w.Header().Set("Content-Type", "application/zip")
	if resp.ContentLength > 0 {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", resp.ContentLength))
		log.Printf("[INFO] Downloading zip %s (size: %d bytes)", path, resp.ContentLength)
	}

	// Stream to both response and cache with buffered copy for better performance
	multiWriter := io.MultiWriter(w, cacheFile)
	startTime := time.Now()

	// Use CopyBuffer with larger buffer for better performance on large files
	buf := make([]byte, 64*1024) // 64KB buffer
	bytesCopied, err := io.CopyBuffer(multiWriter, resp.Body, buf)
	cacheFile.Close()

	if err != nil {
		log.Printf("[ERROR] Error copying zip for %s: %v (copied %d bytes in %v)", path, err, bytesCopied, time.Since(startTime))
		// Remove partial cache file on error
		os.Remove(cachePath + ".tmp")
		// Note: Response may already be partially written, but that's acceptable
		return
	}

	log.Printf("[SUCCESS] Cached zip %s (%d bytes in %v)", path, bytesCopied, time.Since(startTime))

	// Atomically rename temp file to final cache file
	if err := os.Rename(cachePath+".tmp", cachePath); err != nil {
		log.Printf("[WARN] Failed to rename cache file for %s: %v", path, err)
		os.Remove(cachePath + ".tmp")
	}
}
