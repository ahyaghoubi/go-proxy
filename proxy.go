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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
	"github.com/vbauerster/mpb/v8"
	"github.com/vbauerster/mpb/v8/decor"
	"golang.org/x/net/proxy"
)

// upstreamStatusError carries the HTTP status from upstream for propagation to the client
type upstreamStatusError struct {
	status int
}

func (e *upstreamStatusError) Error() string {
	return fmt.Sprintf("upstream returned %d", e.status)
}

// isStderrTTY returns true if stderr is a terminal (supports in-place progress updates)
func isStderrTTY() bool {
	fi, err := os.Stderr.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// progressReader wraps an io.Reader and reports download progress to an mpb bar when enabled.
type progressReader struct {
	reader io.ReadCloser
	bar    *mpb.Bar
}

// newProgressReader creates a reader that reports progress to the given mpb
// progress container (nil to disable). When total <= 0, no bar is created.
// completed allows the bar to start at a non-zero offset (for resumed downloads).
func newProgressReader(r io.ReadCloser, total, completed int64, path string, progress *mpb.Progress) *progressReader {
	pr := &progressReader{reader: r}
	if progress == nil || total <= 0 {
		return pr
	}

	name := path
	if len(name) > 60 {
		name = "..." + name[len(name)-57:]
	}

	if completed < 0 {
		completed = 0
	}
	if completed > total {
		completed = total
	}

	bar := progress.New(total,
		mpb.BarStyle().Rbound("|"),
		mpb.PrependDecorators(
			decor.Name(name, decor.WC{C: decor.DindentRight | decor.DextraSpace}),
		),
		mpb.AppendDecorators(appendDownloadBarDecorators()...),
	)

	// Wrap the reader with bar.ProxyReader so increments are handled internally.
	if proxyReader := bar.ProxyReader(r); proxyReader != nil {
		pr.reader = proxyReader
		if completed > 0 {
			bar.IncrInt64(completed)
		}
		pr.bar = bar
	}
	return pr
}

func (pr *progressReader) Read(p []byte) (n int, err error) {
	return pr.reader.Read(p)
}

func (pr *progressReader) Close() {
	if pr.reader != nil {
		_ = pr.reader.Close()
	}
}

// appendDownloadBarDecorators returns mpb append decorators with explicit labels
// so the bar reads like: " 11% | ETA 49m56s | 30.44 KiB/s ]"
func appendDownloadBarDecorators() []decor.Decorator {
	return []decor.Decorator{
		decor.Percentage(),
		decor.Name(" | ETA "),
		decor.EwmaETA(decor.ET_STYLE_GO, 30),
		decor.Name(" | "),
		decor.EwmaSpeed(decor.SizeB1024(0), "% .2f", 30),
		decor.Name(" ]"),
	}
}

// chunkWriter streams a range into a file at a fixed starting offset using WriteAt.
// It is safe to use concurrently from multiple goroutines as long as ranges do not overlap.
type chunkWriter struct {
	f   *os.File
	off int64
}

func (w *chunkWriter) Write(p []byte) (int, error) {
	n, err := w.f.WriteAt(p, w.off)
	w.off += int64(n)
	return n, err
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
	cacheDir          string
	upstreams         []string
	privateUpstreams  []PrivateUpstream
	gosumdb           string
	gonosumdb         []string
	retryAttempts        int
	retryBackoff         time.Duration
	downloadConnections  int
	maxConcurrentDownloads int
	downloadSem          chan struct{}
	client               *http.Client
	usageStore           *UsageStore
	progress             *mpb.Progress
	mu                   sync.RWMutex
	inFlightMu           sync.Mutex
	inFlight             map[string]*inFlightFetch
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

	downloadConns := cfg.DownloadConnections
	if downloadConns <= 0 {
		downloadConns = 4
	}

	maxConc := cfg.MaxConcurrentDownloads
	if maxConc < 0 {
		maxConc = 0
	}

	p := newProxyWithClient(
		cfg.CacheDir,
		upstreams,
		cfg.PrivateUpstreams,
		cfg.GOSUMDB,
		gonosumdb,
		cfg.RetryAttempts,
		retryBackoff,
		downloadConns,
		maxConc,
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
	downloadConnections int,
	maxConcurrentDownloads int,
	httpProxy, dnsServer string,
	maxCacheAge, cleanupInterval time.Duration,
) *Proxy {
	transport := createTransport(httpProxy, dnsServer, downloadConnections)

	var downloadSem chan struct{}
	if maxConcurrentDownloads > 0 {
		downloadSem = make(chan struct{}, maxConcurrentDownloads)
	}

	var progress *mpb.Progress
	if isStderrTTY() {
		progress = mpb.New(
			mpb.WithOutput(os.Stderr),
			mpb.WithRefreshRate(100*time.Millisecond),
		)
	}

	p := &Proxy{
		cacheDir:          cacheDir,
		upstreams:         upstreams,
		privateUpstreams:  privateUpstreams,
		gosumdb:           gosumdb,
		gonosumdb:         gonosumdb,
		retryAttempts:        retryAttempts,
		retryBackoff:         retryBackoff,
		downloadConnections:  downloadConnections,
		maxConcurrentDownloads: maxConcurrentDownloads,
		downloadSem:          downloadSem,
		client: &http.Client{
			Timeout:   30 * time.Minute,
			Transport: transport,
		},
		progress: progress,
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
		if maxCacheAge > 0 && cleanupInterval > 0 {
			go usageStore.RunCleanup(maxCacheAge, cleanupInterval)
		}
	}

	return p
}

// createTransport builds an http.Transport from proxy and DNS config
func createTransport(httpProxy, dnsServer string, maxConnsPerHost int) *http.Transport {
	if maxConnsPerHost <= 0 {
		maxConnsPerHost = 10
	}
	if maxConnsPerHost < 10 {
		maxConnsPerHost = 10
	}
	if maxConnsPerHost > 100 {
		maxConnsPerHost = 100
	}

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
		MaxIdleConnsPerHost:   maxConnsPerHost,
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
		3, 100*time.Millisecond, 1, 0,
		httpProxy, dnsServer, maxCacheAge, cleanupInterval,
	)
}

// Shutdown stops background cleanup, progress UI, and closes the usage store.
func (p *Proxy) Shutdown() {
	if p.progress != nil {
		p.progress.Wait()
	}
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

const minSizeForParallelDownload = 512 * 1024 // 512KB

// getZipContentLength returns the upstream base URL, auth, content length if known, and whether parallel download is possible.
func (p *Proxy) getZipContentLength(ctx context.Context, requestPath string) (baseURL string, auth *AuthConfig, contentLength int64, ok bool) {
	baseURL, auth = p.selectUpstream(requestPath)
	var upstreams []string
	if auth != nil {
		upstreams = []string{baseURL}
	} else {
		upstreams = p.upstreams
		if len(upstreams) == 0 {
			upstreams = []string{"https://proxy.golang.org"}
		}
	}

	for _, u := range upstreams {
		u = strings.TrimSuffix(u, "/")
		fetchURL := u + "/" + requestPath

		// Try HEAD first
		headReq, err := http.NewRequestWithContext(ctx, http.MethodHead, fetchURL, nil)
		if err != nil {
			continue
		}
		addAuthHeaders(headReq, auth)
		resp, err := p.client.Do(headReq)
		if err == nil && resp.StatusCode == http.StatusOK && resp.ContentLength > 0 {
			resp.Body.Close()
			return u, auth, resp.ContentLength, true
		}
		if resp != nil {
			resp.Body.Close()
		}

		// Try Range 0-0 to get total from Content-Range
		rangeReq, err := http.NewRequestWithContext(ctx, http.MethodGet, fetchURL, nil)
		if err != nil {
			continue
		}
		addAuthHeaders(rangeReq, auth)
		rangeReq.Header.Set("Range", "bytes=0-0")
		resp, err = p.doWithRetry(rangeReq)
		if err != nil {
			continue
		}
		if resp.StatusCode == http.StatusPartialContent {
			cr := resp.Header.Get("Content-Range")
			resp.Body.Close()
			// Format: "bytes 0-0/12345"
			if idx := strings.LastIndex(cr, "/"); idx >= 0 && idx+1 < len(cr) {
				if total, err := strconv.ParseInt(strings.TrimSpace(cr[idx+1:]), 10, 64); err == nil && total > 0 {
					return u, auth, total, true
				}
			}
		} else if resp != nil {
			resp.Body.Close()
		}
	}
	return baseURL, auth, 0, false
}

// fetchRange fetches a byte range from the given upstream. Caller must close the response body.
func (p *Proxy) fetchRange(ctx context.Context, baseURL, requestPath string, start, end int64, auth *AuthConfig) (*http.Response, error) {
	fetchURL := strings.TrimSuffix(baseURL, "/") + "/" + requestPath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fetchURL, nil)
	if err != nil {
		return nil, err
	}
	addAuthHeaders(req, auth)
	// HTTP Range is inclusive: bytes=0-499 means bytes 0 through 499
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end-1))
	return p.doWithRetry(req)
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
		// Global limit on concurrent package downloads (zip fetches)
		if p.downloadSem != nil {
			select {
			case p.downloadSem <- struct{}{}:
				defer func() { <-p.downloadSem }()
			case <-r.Context().Done():
				return "", r.Context().Err()
			}
		}

		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
		defer cancel()

		if err := os.MkdirAll(filepath.Dir(cachePath), 0755); err != nil {
			return "", err
		}
		tmpPath := cachePath + ".tmp"

		baseURL, auth, contentLength, useParallel := p.getZipContentLength(ctx, requestPath)
		useParallel = useParallel && contentLength >= minSizeForParallelDownload && p.downloadConnections > 1

		if useParallel {
			// Parallel download via Range requests, with simple resume support.
			var tmpFile *os.File
			var haveExisting bool
			if fi, err := os.Stat(tmpPath); err == nil && fi.Size() == contentLength {
				tmpFile, err = os.OpenFile(tmpPath, os.O_RDWR, 0644)
				if err != nil {
					return "", err
				}
				haveExisting = true
			} else {
				// Start fresh.
				_ = os.Remove(tmpPath)
				var err error
				tmpFile, err = os.Create(tmpPath)
				if err != nil {
					return "", err
				}
				if err := tmpFile.Truncate(contentLength); err != nil {
					tmpFile.Close()
					os.Remove(tmpPath)
					return "", err
				}
			}

			var bar *mpb.Bar
			if p.progress != nil {
				name := requestPath
				if len(name) > 60 {
					name = "..." + name[len(name)-57:]
				}
				bar = p.progress.New(contentLength,
					mpb.BarStyle().Rbound("|"),
					mpb.PrependDecorators(
						decor.Name(name, decor.WC{C: decor.DindentRight | decor.DextraSpace}),
					),
					mpb.AppendDecorators(appendDownloadBarDecorators()...),
				)
			}

			type chunk struct {
				start int64
				end   int64
				skip  bool
			}

			chunkSize := (contentLength + int64(p.downloadConnections) - 1) / int64(p.downloadConnections)
			var (
				chunks          []chunk
				initialComplete int64
			)
			for i := 0; i < p.downloadConnections; i++ {
				start := int64(i) * chunkSize
				end := start + chunkSize
				if end > contentLength {
					end = contentLength
				}
				if start >= end {
					continue
				}

				ch := chunk{start: start, end: end}
				if haveExisting {
					// Treat a chunk as completed if both its first and last bytes are non-zero.
					buf := make([]byte, 1)
					_, err1 := tmpFile.ReadAt(buf, start)
					first := err1 == nil && buf[0] != 0
					_, err2 := tmpFile.ReadAt(buf, end-1)
					last := err2 == nil && buf[0] != 0
					if first && last {
						ch.skip = true
						initialComplete += end - start
					}
				}
				chunks = append(chunks, ch)
			}

			if bar != nil && initialComplete > 0 {
				bar.IncrInt64(initialComplete)
			}

			var wg sync.WaitGroup
			errs := make([]error, len(chunks))
			for i, ch := range chunks {
				if ch.skip {
					continue
				}
				wg.Add(1)
				go func(i int, ch chunk) {
					defer wg.Done()
					resp, err := p.fetchRange(ctx, baseURL, requestPath, ch.start, ch.end, auth)
					if err != nil {
						errs[i] = err
						return
					}
					defer resp.Body.Close()
					if resp.StatusCode != http.StatusPartialContent {
						errs[i] = fmt.Errorf("expected 206, got %d", resp.StatusCode)
						return
					}

					var reader io.Reader = resp.Body
					var proxy io.ReadCloser
					if bar != nil {
						// Wrap this range reader so mpb can track bytes as they stream in.
						proxy = bar.ProxyReader(resp.Body)
						if proxy != nil {
							reader = proxy
							defer proxy.Close()
						}
					}

					w := &chunkWriter{f: tmpFile, off: ch.start}
					if _, err := io.Copy(w, reader); err != nil {
						errs[i] = err
						return
					}
				}(i, ch)
			}
			wg.Wait()
			// If there were errors, abort the bar so it doesn't leak.
			var hadErr bool
			for _, e := range errs {
				if e != nil {
					hadErr = true
					break
				}
			}
			if bar != nil {
				if hadErr {
					bar.Abort(true)
				} else {
					bar.SetTotal(contentLength, true)
				}
			}
			if err := tmpFile.Close(); err != nil {
				os.Remove(tmpPath)
				return "", err
			}
			for _, e := range errs {
				if e != nil {
					os.Remove(tmpPath)
					return "", e
				}
			}
		} else {
			// Single-connection download with basic resume support.
			partPath := cachePath + ".part"
			tmpPath = partPath

			var existingSize int64
			if fi, err := os.Stat(partPath); err == nil {
				existingSize = fi.Size()
			}

			// Only attempt resume when we know the total size and the partial is smaller.
			if contentLength <= 0 || existingSize >= contentLength {
				if existingSize > 0 {
					_ = os.Remove(partPath)
					existingSize = 0
				}
			}

			var resp *http.Response
			// Try to resume using HTTP Range.
			if existingSize > 0 && contentLength > 0 {
				resp, err = p.fetchRange(ctx, baseURL, requestPath, existingSize, contentLength, auth)
				if err != nil || resp.StatusCode != http.StatusPartialContent {
					if resp != nil {
						resp.Body.Close()
					}
					// Fallback to a full fresh download.
					_ = os.Remove(partPath)
					existingSize = 0
					resp = nil
				}
			}

			if resp == nil {
				// Fresh full download when no usable partial exists.
				req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "", nil)
				resp, err = p.fetchFromUpstreams(requestPath, req)
				if err != nil {
					return "", err
				}
				if resp.StatusCode != http.StatusOK {
					resp.Body.Close()
					return "", fmt.Errorf("upstream returned %d", resp.StatusCode)
				}
			}

			defer resp.Body.Close()

			var tmpFile *os.File
			if existingSize > 0 {
				tmpFile, err = os.OpenFile(partPath, os.O_WRONLY|os.O_APPEND, 0644)
			} else {
				tmpFile, err = os.Create(partPath)
			}
			if err != nil {
				return "", err
			}

			totalSize := contentLength
			if totalSize <= 0 {
				totalSize = resp.ContentLength
			}

			progressBody := newProgressReader(resp.Body, totalSize, existingSize, requestPath, p.progress)
			buf := make([]byte, 256*1024)
			_, err = io.CopyBuffer(tmpFile, progressBody, buf)
			progressBody.Close()
			tmpFile.Close()
			if err != nil {
				os.Remove(partPath)
				return "", err
			}
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
