package main

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"
	"golang.org/x/net/proxy"
)

// upstreamStatusError carries the HTTP status from upstream for propagation to the client
type upstreamStatusError struct {
	status int
}

func (e *upstreamStatusError) Error() string {
	return fmt.Sprintf("upstream returned %d", e.status)
}

var progressOutputMu sync.Mutex

// isStderrTTY returns true if stderr is a terminal (supports \r for in-place updates)
func isStderrTTY() bool {
	fi, err := os.Stderr.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// progressReader wraps an io.Reader and reports download progress
type progressReader struct {
	reader         io.Reader
	total          int64
	downloaded     atomic.Int64
	path           string
	startTime      time.Time
	progressTicker *time.Ticker
	done           chan struct{}
}

// newProgressReader creates a reader that logs progress at regular intervals
func newProgressReader(r io.Reader, total int64, path string) *progressReader {
	pr := &progressReader{
		reader:    r,
		total:     total,
		path:      path,
		startTime: time.Now(),
		done:      make(chan struct{}),
	}
	pr.downloaded.Store(0)

	// Log progress every 500ms
	pr.progressTicker = time.NewTicker(500 * time.Millisecond)
	go pr.logProgress()
	return pr
}

func (pr *progressReader) Read(p []byte) (n int, err error) {
	n, err = pr.reader.Read(p)
	pr.downloaded.Add(int64(n))
	return n, err
}

func (pr *progressReader) Close() {
	pr.progressTicker.Stop()
	close(pr.done)
	// Finalize the progress line with newline so next log appears below (TTY: \r line has no \n yet)
	progressOutputMu.Lock()
	if isStderrTTY() {
		fmt.Fprint(os.Stderr, "\n")
	}
	os.Stderr.Sync()
	progressOutputMu.Unlock()
}

func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

func (pr *progressReader) logProgress() {
	for {
		select {
		case <-pr.done:
			return
		case <-pr.progressTicker.C:
			downloaded := pr.downloaded.Load()
			elapsed := time.Since(pr.startTime).Seconds()
			speed := float64(0)
			if elapsed > 0 {
				speed = float64(downloaded) / elapsed
			}

			var progressLine string
			if pr.total > 0 {
				percent := float64(downloaded) / float64(pr.total) * 100
				remaining := pr.total - downloaded
				eta := time.Duration(0)
				if speed > 0 && remaining > 0 {
					eta = time.Duration(float64(remaining)/speed) * time.Second
				}

				// Progress bar: 20 chars
				barWidth := 20
				filled := int(percent / 100 * float64(barWidth))
				if filled > barWidth {
					filled = barWidth
				}
				bar := strings.Repeat("=", filled) + ">" + strings.Repeat(" ", barWidth-filled)
				if filled == barWidth {
					bar = strings.Repeat("=", barWidth)
				}

				progressLine = fmt.Sprintf("[%s] %5.1f%% | %s/s | %s left | ETA %v",
					bar, percent, formatBytes(int64(speed)), formatBytes(remaining), eta.Round(time.Second))
			} else {
				progressLine = fmt.Sprintf("downloaded %s | %s/s",
					formatBytes(downloaded), formatBytes(int64(speed)))
			}

			// In-place update: overwrite same line until download finishes (\r = carriage return)
			// Use \r for TTY (stays at bottom), \n for non-TTY (e.g. Docker logs) so updates appear live
			progressOutputMu.Lock()
			if isStderrTTY() {
				fmt.Fprintf(os.Stderr, "\r[DOWNLOAD] %s | %s    ", pr.path, progressLine)
			} else {
				fmt.Fprintf(os.Stderr, "[DOWNLOAD] %s | %s\n", pr.path, progressLine)
			}
			os.Stderr.Sync() // Flush so output appears live
			progressOutputMu.Unlock()
		}
	}
}

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

// inFlightFetch holds state for in-flight request deduplication
type inFlightFetch struct {
	cond     *sync.Cond
	done     bool
	byteData []byte
	filePath string
	err      error
}

// Proxy handles Go module proxy requests with disk caching
type Proxy struct {
	cacheDir        string
	upstreams       []string
	privateUpstreams []PrivateUpstream
	gosumdb         string
	gonosumdb       []string
	retryAttempts   int
	retryBackoff    time.Duration
	client          *http.Client
	usageStore      *UsageStore
	mu              sync.RWMutex
	inFlightMu      sync.Mutex
	inFlight        map[string]*inFlightFetch
}

// NewProxyFromConfig creates a proxy from Config with all features (upstreams, retry, dedup, etc.)
func NewProxyFromConfig(cfg *Config, maxCacheAge, cleanupInterval, retryBackoff time.Duration) *Proxy {
	upstreams := cfg.Upstreams
	if len(upstreams) == 0 {
		upstreams = []string{"https://proxy.golang.org"}
	}
	for i, u := range upstreams {
		upstreams[i] = strings.TrimSuffix(u, "/")
	}

	gonosumdb := parseGONOSUMDB(cfg.GONOSUMDB)

	p := newProxyWithClient(
		cfg.CacheDir,
		upstreams,
		cfg.PrivateUpstreams,
		cfg.GOSUMDB,
		gonosumdb,
		cfg.RetryAttempts,
		retryBackoff,
		cfg.Proxy,
		cfg.DNSServer,
		maxCacheAge,
		cleanupInterval,
	)
	return p
}

func parseGONOSUMDB(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// newProxyWithClient creates a Proxy with the given parameters and HTTP client (shared by NewProxy/NewProxyFromConfig)
func newProxyWithClient(
	cacheDir string,
	upstreams []string,
	privateUpstreams []PrivateUpstream,
	gosumdb string,
	gonosumdb []string,
	retryAttempts int,
	retryBackoff time.Duration,
	httpProxy, dnsServer string,
	maxCacheAge, cleanupInterval time.Duration,
) *Proxy {
	transport := createTransport(httpProxy, dnsServer)

	p := &Proxy{
		cacheDir:        cacheDir,
		upstreams:       upstreams,
		privateUpstreams: privateUpstreams,
		gosumdb:         gosumdb,
		gonosumdb:       gonosumdb,
		retryAttempts:   retryAttempts,
		retryBackoff:    retryBackoff,
		client: &http.Client{
			Timeout:   5 * time.Minute,
			Transport: transport,
		},
		inFlight: make(map[string]*inFlightFetch),
	}

	if retryAttempts <= 0 {
		p.retryAttempts = 3
	}
	if retryBackoff <= 0 {
		p.retryBackoff = 100 * time.Millisecond
	}

	usageStore, err := NewUsageStore(cacheDir)
	if err != nil {
		log.Printf("[WARN] Failed to initialize usage store: %v (cache TTL cleanup disabled)", err)
	} else {
		p.usageStore = usageStore
		go usageStore.RunCleanup(maxCacheAge, cleanupInterval)
	}

	return p
}

// createTransport builds an http.Transport from proxy and DNS config
func createTransport(httpProxy, dnsServer string) *http.Transport {
	dnsResolver, err := createDNSResolver(dnsServer)
	if err != nil {
		log.Printf("[WARN] Failed to create DNS resolver: %v", err)
		dnsResolver = nil
	} else if dnsResolver != nil {
		log.Printf("Using DNS resolver: %s", dnsServer)
	}

	dialer := createDialer(dnsResolver)

	transport := &http.Transport{
		DialContext:           dialer,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
	}

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

	if proxyURL != "" {
		parsedURL, err := url.Parse(proxyURL)
		if err != nil {
			log.Printf("[WARN] Invalid proxy URL '%s': %v", proxyURL, err)
		} else {
			switch parsedURL.Scheme {
			case "http", "https":
				transport.Proxy = http.ProxyURL(parsedURL)
				log.Printf("Using HTTP proxy: %s", proxyURL)
			case "socks5", "socks5h":
				socksDialer, err := proxy.SOCKS5("tcp", parsedURL.Host, nil, proxy.Direct)
				if err != nil {
					log.Printf("[WARN] Failed to create SOCKS5 dialer: %v", err)
				} else {
					if dnsResolver != nil {
						transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
							host, port, err := net.SplitHostPort(address)
							if err != nil {
								return nil, err
							}
							ips, err := dnsResolver.LookupIP(ctx, host)
							if err != nil || len(ips) == 0 {
								return nil, fmt.Errorf("failed to resolve %s: %v", host, err)
							}
							resolvedAddr := net.JoinHostPort(ips[0].String(), port)
							return socksDialer.(proxy.ContextDialer).DialContext(ctx, network, resolvedAddr)
						}
					} else {
						transport.DialContext = socksDialer.(proxy.ContextDialer).DialContext
					}
					log.Printf("Using SOCKS5 proxy: %s", proxyURL)
				}
			default:
				log.Printf("[WARN] Unsupported proxy scheme: %s", parsedURL.Scheme)
			}
		}
	}

	return transport
}

// NewProxy creates a new proxy instance (backward compatible; uses single upstream)
func NewProxy(cacheDir, upstream, httpProxy, dnsServer string, maxCacheAge, cleanupInterval time.Duration) *Proxy {
	upstreams := []string{strings.TrimSuffix(upstream, "/")}
	return newProxyWithClient(
		cacheDir, upstreams, nil, "sum.golang.org", nil,
		3, 100*time.Millisecond,
		httpProxy, dnsServer, maxCacheAge, cleanupInterval,
	)
}

// Shutdown stops background cleanup and closes the usage store.
func (p *Proxy) Shutdown() {
	if p.usageStore != nil {
		p.usageStore.Stop()
		_ = p.usageStore.Close()
	}
}

// selectUpstream returns the base URL and optional auth for the given path
func (p *Proxy) selectUpstream(requestPath string) (baseURL string, auth *AuthConfig) {
	module := pathToModule(requestPath)
	for _, pu := range p.privateUpstreams {
		pattern := strings.TrimSpace(pu.Pattern)
		if pattern == "" {
			continue
		}
		matched, err := path.Match(pattern, module)
		if err != nil {
			continue
		}
		if matched {
			u := strings.TrimSuffix(pu.URL, "/")
			return u, &pu.Auth
		}
	}
	if len(p.upstreams) > 0 {
		return p.upstreams[0], nil
	}
	return "https://proxy.golang.org", nil
}

// addAuthHeaders adds auth headers to the request
func addAuthHeaders(req *http.Request, auth *AuthConfig) {
	if auth == nil {
		return
	}
	switch strings.ToLower(auth.Type) {
	case "basic":
		if auth.Value != "" {
			encoded := base64.StdEncoding.EncodeToString([]byte(auth.Value))
			req.Header.Set("Authorization", "Basic "+encoded)
		}
	case "bearer":
		if auth.Value != "" {
			req.Header.Set("Authorization", "Bearer "+auth.Value)
		}
	case "header":
		if auth.Name != "" && auth.Value != "" {
			req.Header.Set(auth.Name, auth.Value)
		}
	}
}

// doWithRetry executes the request with retries on transient failures
func (p *Proxy) doWithRetry(req *http.Request) (*http.Response, error) {
	var lastErr error
	backoff := p.retryBackoff
	for attempt := 0; attempt < p.retryAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(backoff)
			backoff *= 2
		}
		resp, err := p.client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		// Retry on 5xx and 429
		if resp.StatusCode >= 500 || resp.StatusCode == 429 {
			status := resp.StatusCode
			resp.Body.Close()
			lastErr = &upstreamStatusError{status: status}
			continue
		}
		return resp, nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("max retries exceeded")
}

// fetchFromUpstreams tries each upstream until success; 404/410 try next upstream
func (p *Proxy) fetchFromUpstreams(requestPath string, req *http.Request) (*http.Response, error) {
	baseURL, auth := p.selectUpstream(requestPath)
	var upstreams []string
	if auth != nil {
		upstreams = []string{baseURL}
	} else {
		upstreams = p.upstreams
		if len(upstreams) == 0 {
			upstreams = []string{"https://proxy.golang.org"}
		}
	}

	var lastErr error
	for _, u := range upstreams {
		u = strings.TrimSuffix(u, "/")
		fetchURL := u + "/" + requestPath
		fetchReq, err := http.NewRequestWithContext(req.Context(), http.MethodGet, fetchURL, nil)
		if err != nil {
			lastErr = err
			continue
		}
		addAuthHeaders(fetchReq, auth)
		resp, err := p.doWithRetry(fetchReq)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode == 404 || resp.StatusCode == 410 {
			status := resp.StatusCode
			resp.Body.Close()
			lastErr = &upstreamStatusError{status: status}
			continue
		}
		return resp, nil
	}
	return nil, lastErr
}

// fetchWithDedup fetches bytes with in-flight deduplication
func (p *Proxy) fetchWithDedup(requestPath string, fetchFn func() ([]byte, error)) ([]byte, error) {
	p.inFlightMu.Lock()
	if f, ok := p.inFlight[requestPath]; ok {
		for !f.done {
			f.cond.Wait()
		}
		p.inFlightMu.Unlock()
		if f.err != nil {
			return nil, f.err
		}
		return f.byteData, nil
	}
	f := &inFlightFetch{cond: sync.NewCond(&p.inFlightMu)}
	p.inFlight[requestPath] = f
	p.inFlightMu.Unlock()

	f.byteData, f.err = fetchFn()

	p.inFlightMu.Lock()
	f.done = true
	f.cond.Broadcast()
	delete(p.inFlight, requestPath)
	p.inFlightMu.Unlock()

	return f.byteData, f.err
}

// fetchZipWithDedup fetches zip with in-flight deduplication; waiters read from cache when done
func (p *Proxy) fetchZipWithDedup(requestPath, cachePath string, fetchFn func() (string, error)) (string, error) {
	p.inFlightMu.Lock()
	if f, ok := p.inFlight[requestPath]; ok {
		for !f.done {
			f.cond.Wait()
		}
		p.inFlightMu.Unlock()
		if f.err != nil {
			return "", f.err
		}
		return f.filePath, nil
	}
	f := &inFlightFetch{cond: sync.NewCond(&p.inFlightMu)}
	p.inFlight[requestPath] = f
	p.inFlightMu.Unlock()

	filePath, fetchErr := fetchFn()

	p.inFlightMu.Lock()
	f.done = true
	f.filePath = filePath
	f.err = fetchErr
	f.cond.Broadcast()
	delete(p.inFlight, requestPath)
	p.inFlightMu.Unlock()

	return filePath, fetchErr
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
func (p *Proxy) handleList(w http.ResponseWriter, r *http.Request, requestPath string) {
	cachePath := cachePath(p.cacheDir, requestPath)

	// Try cache first (read lock)
	p.mu.RLock()
	cached, err := readCache(cachePath)
	p.mu.RUnlock()

	if err == nil {
		log.Printf("[CACHE HIT] %s", requestPath)
		if p.usageStore != nil {
			p.usageStore.RecordUsage(requestPath)
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write(cached)
		return
	}

	log.Printf("[CACHE MISS] %s", requestPath)

	data, err := p.fetchWithDedup(requestPath, func() ([]byte, error) {
		resp, err := p.fetchFromUpstreams(requestPath, r)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("upstream returned %d", resp.StatusCode)
		}
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}
		p.mu.Lock()
		_ = writeCache(cachePath, data)
		p.mu.Unlock()
		return data, nil
	})

	if err != nil {
		writeFetchError(w, requestPath, err)
		return
	}

	if p.usageStore != nil {
		p.usageStore.RecordUsage(requestPath)
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write(data)
}

func writeFetchError(w http.ResponseWriter, path string, err error) {
	var ue *upstreamStatusError
	if errors.As(err, &ue) {
		log.Printf("[ERROR] Upstream returned %d for %s", ue.status, path)
		http.Error(w, err.Error(), ue.status)
	} else {
		log.Printf("[ERROR] Failed to fetch %s: %v", path, err)
		http.Error(w, fmt.Sprintf("Failed to fetch: %v", err), http.StatusBadGateway)
	}
}

// handleInfo handles GET /<module>/@v/<version>.info requests
func (p *Proxy) handleInfo(w http.ResponseWriter, r *http.Request, requestPath string) {
	cachePath := cachePath(p.cacheDir, requestPath)

	// Try cache first (read lock)
	p.mu.RLock()
	cached, err := readCache(cachePath)
	p.mu.RUnlock()

	if err == nil {
		log.Printf("[CACHE HIT] %s", requestPath)
		if p.usageStore != nil {
			p.usageStore.RecordUsage(requestPath)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(cached)
		return
	}

	log.Printf("[CACHE MISS] %s", requestPath)

	data, err := p.fetchWithDedup(requestPath, func() ([]byte, error) {
		resp, err := p.fetchFromUpstreams(requestPath, r)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("upstream returned %d", resp.StatusCode)
		}
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}
		var info map[string]interface{}
		if err := json.Unmarshal(data, &info); err != nil {
			return nil, fmt.Errorf("invalid JSON: %w", err)
		}
		p.mu.Lock()
		_ = writeCache(cachePath, data)
		p.mu.Unlock()
		return data, nil
	})

	if err != nil {
		writeFetchError(w, requestPath, err)
		return
	}

	if p.usageStore != nil {
		p.usageStore.RecordUsage(requestPath)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

// handleMod handles GET /<module>/@v/<version>.mod requests
func (p *Proxy) handleMod(w http.ResponseWriter, r *http.Request, requestPath string) {
	cachePath := cachePath(p.cacheDir, requestPath)

	// Try cache first (read lock)
	p.mu.RLock()
	cached, err := readCache(cachePath)
	p.mu.RUnlock()

	if err == nil {
		log.Printf("[CACHE HIT] %s", requestPath)
		if p.usageStore != nil {
			p.usageStore.RecordUsage(requestPath)
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write(cached)
		return
	}

	log.Printf("[CACHE MISS] %s", requestPath)

	data, err := p.fetchWithDedup(requestPath, func() ([]byte, error) {
		resp, err := p.fetchFromUpstreams(requestPath, r)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("upstream returned %d", resp.StatusCode)
		}
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}
		p.mu.Lock()
		_ = writeCache(cachePath, data)
		p.mu.Unlock()
		return data, nil
	})

	if err != nil {
		writeFetchError(w, requestPath, err)
		return
	}

	if p.usageStore != nil {
		p.usageStore.RecordUsage(requestPath)
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write(data)
}

// handleZip handles GET /<module>/@v/<version>.zip requests
func (p *Proxy) handleZip(w http.ResponseWriter, r *http.Request, requestPath string) {
	cachePath := cachePath(p.cacheDir, requestPath)

	// Try cache first (read lock)
	p.mu.RLock()
	file, err := os.Open(cachePath)
	p.mu.RUnlock()

	if err == nil {
		defer file.Close()
		stat, err := file.Stat()
		if err == nil {
			log.Printf("[CACHE HIT] %s", requestPath)
			if p.usageStore != nil {
				p.usageStore.RecordUsage(requestPath)
			}
			w.Header().Set("Content-Type", "application/zip")
			w.Header().Set("Content-Length", fmt.Sprintf("%d", stat.Size()))
			io.Copy(w, file)
			return
		}
		file.Close()
	}

	log.Printf("[CACHE MISS] %s", requestPath)

	finalPath, err := p.fetchZipWithDedup(requestPath, cachePath, func() (string, error) {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "", nil)
		resp, err := p.fetchFromUpstreams(requestPath, req)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("upstream returned %d", resp.StatusCode)
		}

		if err := os.MkdirAll(filepath.Dir(cachePath), 0755); err != nil {
			return "", err
		}
		tmpPath := cachePath + ".tmp"
		tmpFile, err := os.Create(tmpPath)
		if err != nil {
			return "", err
		}
		totalSize := resp.ContentLength
		progressBody := newProgressReader(resp.Body, totalSize, requestPath)
		buf := make([]byte, 64*1024)
		_, err = io.CopyBuffer(tmpFile, progressBody, buf)
		progressBody.Close()
		tmpFile.Close()
		if err != nil {
			os.Remove(tmpPath)
			return "", err
		}

		data, err := os.ReadFile(tmpPath)
		if err != nil {
			os.Remove(tmpPath)
			return "", err
		}

		module, version := pathToModuleAndVersion(requestPath)
		if err := VerifyZip(p.gosumdb, module, version, data, p.gonosumdb); err != nil {
			os.Remove(tmpPath)
			return "", fmt.Errorf("checksum verification failed: %w", err)
		}

		p.mu.Lock()
		if _, err := os.Stat(cachePath); err == nil {
			os.Remove(tmpPath)
			p.mu.Unlock()
			return cachePath, nil
		}
		if err := os.Rename(tmpPath, cachePath); err != nil {
			os.Remove(tmpPath)
			p.mu.Unlock()
			return "", err
		}
		p.mu.Unlock()
		return cachePath, nil
	})

	if err != nil {
		writeFetchError(w, requestPath, err)
		return
	}

	if p.usageStore != nil {
		p.usageStore.RecordUsage(requestPath)
	}

	file, err = os.Open(finalPath)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to read cache: %v", err), http.StatusInternalServerError)
		return
	}
	defer file.Close()
	stat, _ := file.Stat()
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", stat.Size()))
	io.Copy(w, file)
}
