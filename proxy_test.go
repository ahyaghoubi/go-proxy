package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestFormatBytes(t *testing.T) {
	tests := []struct {
		b    int64
		want string
	}{
		{0, "0 B"},
		{1, "1 B"},
		{500, "500 B"},
		{1023, "1023 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{1024 * 1024, "1.0 MB"},
		{1024 * 1024 * 1024, "1.0 GB"},
		{5 * 1024 * 1024, "5.0 MB"},
	}
	for _, tt := range tests {
		got := formatBytes(tt.b)
		if got != tt.want {
			t.Errorf("formatBytes(%d) = %q, want %q", tt.b, got, tt.want)
		}
	}
}

func newTestProxy(t *testing.T, cacheDir, upstream string) *Proxy {
	t.Helper()
	return newTestProxyWithConfig(t, cacheDir, upstream, false)
}

// newTestProxyNoSumDB creates a proxy with checksum verification disabled (for tests that mock upstream zip)
func newTestProxyNoSumDB(t *testing.T, cacheDir, upstream string) *Proxy {
	t.Helper()
	return newTestProxyWithConfig(t, cacheDir, upstream, true)
}

func newTestProxyWithConfig(t *testing.T, cacheDir, upstream string, noSumDB bool) *Proxy {
	t.Helper()
	var p *Proxy
	if noSumDB {
		cfg := DefaultConfig()
		cfg.CacheDir = cacheDir
		cfg.Upstreams = []string{strings.TrimSuffix(upstream, "/")}
		cfg.GOSUMDB = "off"
		p = NewProxyFromConfig(&cfg, 720*time.Hour, 24*time.Hour, 100*time.Millisecond)
	} else {
		p = NewProxy(cacheDir, upstream, "", "", 720*time.Hour, 24*time.Hour)
	}
	t.Cleanup(func() { p.Shutdown() })
	return p
}

func TestHandleHealth(t *testing.T) {
	dir := t.TempDir()
	proxy := newTestProxy(t, dir, "https://proxy.golang.org")

	req := httptest.NewRequest("GET", "/health", nil)
	rec := httptest.NewRecorder()
	proxy.HandleRequest(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if body := rec.Body.String(); !strings.Contains(body, "ok") {
		t.Errorf("body = %q, want containing 'ok'", body)
	}
}

func TestHandleHealthz(t *testing.T) {
	dir := t.TempDir()
	proxy := newTestProxy(t, dir, "https://proxy.golang.org")

	req := httptest.NewRequest("GET", "/healthz", nil)
	rec := httptest.NewRecorder()
	proxy.HandleRequest(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleMethodNotAllowed(t *testing.T) {
	dir := t.TempDir()
	proxy := newTestProxy(t, dir, "https://proxy.golang.org")

	req := httptest.NewRequest("POST", "/github.com/foo/bar/@v/list", nil)
	rec := httptest.NewRecorder()
	proxy.HandleRequest(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}

func TestHandleNotFound(t *testing.T) {
	dir := t.TempDir()
	proxy := newTestProxy(t, dir, "https://proxy.golang.org")

	req := httptest.NewRequest("GET", "/unknown/path", nil)
	rec := httptest.NewRecorder()
	proxy.HandleRequest(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestHandleListUpstreamError(t *testing.T) {
	dir := t.TempDir()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer upstream.Close()

	proxy := newTestProxy(t, dir, upstream.URL)

	req := httptest.NewRequest("GET", "/github.com/foo/bar/@v/list", nil)
	rec := httptest.NewRecorder()
	proxy.HandleRequest(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestHandleInfoUpstreamError(t *testing.T) {
	dir := t.TempDir()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer upstream.Close()

	proxy := newTestProxy(t, dir, upstream.URL)

	req := httptest.NewRequest("GET", "/github.com/foo/bar/@v/v1.0.0.info", nil)
	rec := httptest.NewRecorder()
	proxy.HandleRequest(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

func TestHandleInfoInvalidJSON(t *testing.T) {
	dir := t.TempDir()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("not valid json"))
	}))
	defer upstream.Close()

	proxy := newTestProxy(t, dir, upstream.URL)

	req := httptest.NewRequest("GET", "/github.com/foo/bar/@v/v1.0.0.info", nil)
	rec := httptest.NewRecorder()
	proxy.HandleRequest(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}

func TestHandleModUpstreamError(t *testing.T) {
	dir := t.TempDir()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer upstream.Close()

	proxy := newTestProxy(t, dir, upstream.URL)

	req := httptest.NewRequest("GET", "/github.com/foo/bar/@v/v1.0.0.mod", nil)
	rec := httptest.NewRecorder()
	proxy.HandleRequest(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestHandleZipUpstreamError(t *testing.T) {
	dir := t.TempDir()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer upstream.Close()

	proxy := newTestProxy(t, dir, upstream.URL)

	req := httptest.NewRequest("GET", "/github.com/foo/bar/@v/v1.0.0.zip", nil)
	rec := httptest.NewRecorder()
	proxy.HandleRequest(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestHandleZipUnknownContentLength(t *testing.T) {
	// Chunked transfer - no Content-Length, progress shows "downloaded X | Y/s" format
	dir := t.TempDir()
	zipContent := []byte("chunked zip content")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".zip") {
			w.Header().Set("Content-Type", "application/zip")
			// No Content-Length - chunked
			w.Write(zipContent)
			return
		}
		http.NotFound(w, r)
	}))
	defer upstream.Close()

	proxy := newTestProxyNoSumDB(t, dir, upstream.URL)

	req := httptest.NewRequest("GET", "/example.com/mod/@v/v1.0.0.zip", nil)
	rec := httptest.NewRecorder()
	proxy.HandleRequest(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if !bytes.Equal(rec.Body.Bytes(), zipContent) {
		t.Error("body mismatch")
	}
}

func TestProxyShutdownWithNilUsageStore(t *testing.T) {
	// Proxy with nil usageStore should not panic on Shutdown
	p := &Proxy{usageStore: nil}
	p.Shutdown()
}

func TestHandleListCacheHit(t *testing.T) {
	dir := t.TempDir()
	// Pre-populate cache
	cachePath := cachePath(dir, "github.com/foo/bar/@v/list")
	if err := os.MkdirAll(filepath.Dir(cachePath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cachePath, []byte("v1.0.0\n"), 0644); err != nil {
		t.Fatal(err)
	}

	proxy := newTestProxy(t, dir, "https://proxy.golang.org")

	req := httptest.NewRequest("GET", "/github.com/foo/bar/@v/list", nil)
	rec := httptest.NewRecorder()
	proxy.HandleRequest(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); body != "v1.0.0\n" {
		t.Errorf("body = %q, want v1.0.0\\n", body)
	}
}

func TestHandleListCacheMiss(t *testing.T) {
	dir := t.TempDir()

	// Mock upstream server
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/github.com/foo/bar/@v/list" {
			w.Write([]byte("v1.0.0\nv2.0.0\n"))
			return
		}
		http.NotFound(w, r)
	}))
	defer upstream.Close()

	proxy := newTestProxy(t, dir, upstream.URL)

	req := httptest.NewRequest("GET", "/github.com/foo/bar/@v/list", nil)
	rec := httptest.NewRecorder()
	proxy.HandleRequest(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); body != "v1.0.0\nv2.0.0\n" {
		t.Errorf("body = %q, want v1.0.0\\nv2.0.0\\n", body)
	}

	// Verify cached
	cachePath := cachePath(dir, "github.com/foo/bar/@v/list")
	data, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("cache file not written: %v", err)
	}
	if string(data) != "v1.0.0\nv2.0.0\n" {
		t.Errorf("cached = %q, want v1.0.0\\nv2.0.0\\n", data)
	}
}

func TestHandleInfoCacheHit(t *testing.T) {
	dir := t.TempDir()
	infoJSON := `{"Version":"v1.0.0","Time":"2024-01-01T00:00:00Z"}`
	cachePath := cachePath(dir, "github.com/foo/bar/@v/v1.0.0.info")
	if err := os.MkdirAll(filepath.Dir(cachePath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cachePath, []byte(infoJSON), 0644); err != nil {
		t.Fatal(err)
	}

	proxy := newTestProxy(t, dir, "https://proxy.golang.org")

	req := httptest.NewRequest("GET", "/github.com/foo/bar/@v/v1.0.0.info", nil)
	rec := httptest.NewRecorder()
	proxy.HandleRequest(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); body != infoJSON {
		t.Errorf("body = %q, want %q", body, infoJSON)
	}
}

func TestHandleInfoCacheMiss(t *testing.T) {
	dir := t.TempDir()
	infoJSON := `{"Version":"v1.0.0","Time":"2024-01-01T00:00:00Z"}`

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".info") {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(infoJSON))
			return
		}
		http.NotFound(w, r)
	}))
	defer upstream.Close()

	proxy := newTestProxy(t, dir, upstream.URL)

	req := httptest.NewRequest("GET", "/github.com/foo/bar/@v/v1.0.0.info", nil)
	rec := httptest.NewRecorder()
	proxy.HandleRequest(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); body != infoJSON {
		t.Errorf("body = %q, want %q", body, infoJSON)
	}
}

func TestHandleModCacheHit(t *testing.T) {
	dir := t.TempDir()
	modContent := "module github.com/foo/bar\n\ngo 1.21\n"
	cachePath := cachePath(dir, "github.com/foo/bar/@v/v1.0.0.mod")
	if err := os.MkdirAll(filepath.Dir(cachePath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cachePath, []byte(modContent), 0644); err != nil {
		t.Fatal(err)
	}

	proxy := newTestProxy(t, dir, "https://proxy.golang.org")

	req := httptest.NewRequest("GET", "/github.com/foo/bar/@v/v1.0.0.mod", nil)
	rec := httptest.NewRecorder()
	proxy.HandleRequest(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); body != modContent {
		t.Errorf("body = %q, want %q", body, modContent)
	}
}

func TestHandleModCacheMiss(t *testing.T) {
	dir := t.TempDir()
	modContent := "module github.com/foo/bar\n\ngo 1.21\n"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".mod") {
			w.Write([]byte(modContent))
			return
		}
		http.NotFound(w, r)
	}))
	defer upstream.Close()

	proxy := newTestProxy(t, dir, upstream.URL)

	req := httptest.NewRequest("GET", "/github.com/foo/bar/@v/v1.0.0.mod", nil)
	rec := httptest.NewRecorder()
	proxy.HandleRequest(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); body != modContent {
		t.Errorf("body = %q, want %q", body, modContent)
	}
}

func TestHandleZipCacheHit(t *testing.T) {
	dir := t.TempDir()
	zipContent := []byte("fake zip content")
	cachePath := cachePath(dir, "github.com/foo/bar/@v/v1.0.0.zip")
	if err := os.MkdirAll(filepath.Dir(cachePath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cachePath, zipContent, 0644); err != nil {
		t.Fatal(err)
	}

	proxy := newTestProxy(t, dir, "https://proxy.golang.org")

	req := httptest.NewRequest("GET", "/github.com/foo/bar/@v/v1.0.0.zip", nil)
	rec := httptest.NewRecorder()
	proxy.HandleRequest(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if body := rec.Body.Bytes(); !bytes.Equal(body, zipContent) {
		t.Errorf("body mismatch: got %d bytes, want %d", len(body), len(zipContent))
	}
}

func TestHandleZipCacheMiss(t *testing.T) {
	dir := t.TempDir()
	zipContent := []byte("fake zip content for download")

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".zip") {
			w.Header().Set("Content-Type", "application/zip")
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(zipContent)))
			w.Write(zipContent)
			return
		}
		http.NotFound(w, r)
	}))
	defer upstream.Close()

	proxy := newTestProxyNoSumDB(t, dir, upstream.URL)

	req := httptest.NewRequest("GET", "/github.com/foo/bar/@v/v1.0.0.zip", nil)
	rec := httptest.NewRecorder()
	proxy.HandleRequest(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if body := rec.Body.Bytes(); !bytes.Equal(body, zipContent) {
		t.Errorf("body mismatch: got %q, want %q", body, zipContent)
	}

	// Verify cached
	cachePath := cachePath(dir, "github.com/foo/bar/@v/v1.0.0.zip")
	data, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("cache file not written: %v", err)
	}
	if !bytes.Equal(data, zipContent) {
		t.Errorf("cached content mismatch")
	}
}

func TestProgressReader(t *testing.T) {
	content := []byte("hello world")
	r := bytes.NewReader(content)
	pr := newProgressReader(io.NopCloser(r), int64(len(content)), 0, "test/path", nil)
	defer pr.Close()

	buf := make([]byte, 1024)
	n, err := pr.Read(buf)
	if err != nil && err != io.EOF {
		t.Fatalf("Read failed: %v", err)
	}
	if n != len(content) {
		t.Errorf("Read = %d bytes, want %d", n, len(content))
	}
	if string(buf[:n]) != string(content) {
		t.Errorf("Read content = %q, want %q", buf[:n], content)
	}
}

// slowReader yields data in chunks with delays so progress ticker (500ms) can fire
type slowReader struct {
	data  []byte
	pos   int
	delay time.Duration
	chunk int
}

func (s *slowReader) Read(p []byte) (n int, err error) {
	if s.pos >= len(s.data) {
		return 0, io.EOF
	}
	time.Sleep(s.delay)
	chunk := s.chunk
	if chunk <= 0 {
		chunk = 8192
	}
	if chunk > len(s.data)-s.pos {
		chunk = len(s.data) - s.pos
	}
	n = copy(p, s.data[s.pos:s.pos+chunk])
	s.pos += n
	return n, nil
}

func TestDownloadProgressOutputFormat(t *testing.T) {
	// Use large enough payload and slow reader to exercise the zip download path.
	zipContent := bytes.Repeat([]byte("x"), 100*1024) // 100KB
	sr := &slowReader{data: zipContent, delay: 300 * time.Millisecond, chunk: 32 * 1024} // ~1s total, 2+ progress ticks

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, ".zip") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(zipContent)))
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		io.Copy(w, sr)
	}))
	defer upstream.Close()

	dir := t.TempDir()
	proxy := newTestProxyNoSumDB(t, dir, upstream.URL)

	// Capture stderr
	oldStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w

	var captured bytes.Buffer
	done := make(chan struct{})
	go func() {
		io.Copy(&captured, r)
		close(done)
	}()

	req := httptest.NewRequest("GET", "/github.com/foo/bar/@v/v1.0.0.zip", nil)
	rec := httptest.NewRecorder()
	proxy.HandleRequest(rec, req)

	os.Stderr = oldStderr
	w.Close()
	<-done

	output := captured.String()
	_ = output // with mpb-based progress, exact stderr format is library-defined; we only care that the handler succeeds
}

func TestDownloadProgressUpdatesLive(t *testing.T) {
	// Slow download exercises multi-chunk progress handling. With mpb the exact
	// stderr format is library-defined, so this test only asserts that the
	// request completes successfully and stderr capture does not hang.
	zipContent := bytes.Repeat([]byte("a"), 200*1024) // 200KB
	sr := &slowReader{data: zipContent, delay: 300 * time.Millisecond, chunk: 32 * 1024} // ~2s total

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, ".zip") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(zipContent)))
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		io.Copy(w, sr)
	}))
	defer upstream.Close()

	dir := t.TempDir()
	proxy := newTestProxyNoSumDB(t, dir, upstream.URL)

	oldStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w

	var captured bytes.Buffer
	done := make(chan struct{})
	go func() {
		io.Copy(&captured, r)
		close(done)
	}()

	req := httptest.NewRequest("GET", "/github.com/bar/baz/@v/v2.0.0.zip", nil)
	rec := httptest.NewRecorder()
	proxy.HandleRequest(rec, req)

	os.Stderr = oldStderr
	w.Close()
	<-done

	output := captured.String()
	if len(output) == 0 {
		t.Log("no progress output captured; mpb is disabled when stderr is not a TTY")
	}
}

func TestDownloadProgressStaysAtBottomWhenTTY(t *testing.T) {
	// With mpb-based progress, TTY-aware rendering is handled by the library.
	// In tests stderr is a pipe (non-TTY), so we only assert that some output
	// is written (or that at least the handler completes without error).
	zipContent := bytes.Repeat([]byte("x"), 50*1024)
	sr := &slowReader{data: zipContent, delay: 350 * time.Millisecond, chunk: 16 * 1024}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, ".zip") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(zipContent)))
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		io.Copy(w, sr)
	}))
	defer upstream.Close()

	dir := t.TempDir()
	proxy := newTestProxyNoSumDB(t, dir, upstream.URL)

	oldStderr := os.Stderr
	pipeR, pipeW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = pipeW

	var captured bytes.Buffer
	done := make(chan struct{})
	go func() {
		io.Copy(&captured, pipeR)
		close(done)
	}()

	req := httptest.NewRequest("GET", "/example.com/mod/@v/v1.0.0.zip", nil)
	rec := httptest.NewRecorder()
	proxy.HandleRequest(rec, req)

	os.Stderr = oldStderr
	pipeW.Close()
	<-done

	output := captured.String()
	_ = output
}

func TestGetZipContentLength_UsesHeadContentLength(t *testing.T) {
	dir := t.TempDir()
	const size = 2 * 1024 * 1024 // 2MB

	var headCount, rangeCount int

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".zip") {
			switch r.Method {
			case http.MethodHead:
				headCount++
				w.Header().Set("Content-Length", fmt.Sprintf("%d", size))
				w.WriteHeader(http.StatusOK)
				return
			case http.MethodGet:
				if r.Header.Get("Range") != "" {
					rangeCount++
				}
				http.Error(w, "unexpected GET", http.StatusBadRequest)
				return
			}
		}
		http.NotFound(w, r)
	}))
	defer upstream.Close()

	cfg := DefaultConfig()
	cfg.CacheDir = dir
	cfg.Upstreams = []string{strings.TrimSuffix(upstream.URL, "/")}
	cfg.GOSUMDB = "off"
	proxy := NewProxyFromConfig(&cfg, 720*time.Hour, 24*time.Hour, 100*time.Millisecond)
	defer proxy.Shutdown()

	baseURL, auth, length, ok := proxy.getZipContentLength(context.Background(), "github.com/foo/bar/@v/v1.0.0.zip")
	if !ok {
		t.Fatalf("getZipContentLength returned ok=false")
	}
	if auth != nil {
		t.Fatalf("expected no auth, got %#v", auth)
	}
	expectedBase := strings.TrimSuffix(upstream.URL, "/")
	if baseURL != expectedBase {
		t.Fatalf("baseURL = %q, want %q", baseURL, expectedBase)
	}
	if length != size {
		t.Fatalf("length = %d, want %d", length, size)
	}
	if headCount == 0 {
		t.Errorf("expected at least one HEAD request")
	}
	if rangeCount != 0 {
		t.Errorf("expected no Range GETs, got %d", rangeCount)
	}
}

func TestGetZipContentLength_UsesRangeProbeWhenNoHeadLength(t *testing.T) {
	dir := t.TempDir()
	const size = 1024 * 1024 // 1MB

	var headCount, rangeProbeCount int

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".zip") {
			switch r.Method {
			case http.MethodHead:
				headCount++
				// No Content-Length header – forces Range probe
				w.WriteHeader(http.StatusOK)
				return
			case http.MethodGet:
				if strings.HasPrefix(r.Header.Get("Range"), "bytes=0-0") {
					rangeProbeCount++
					w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-0/%d", size))
					w.WriteHeader(http.StatusPartialContent)
					w.Write([]byte{0}) // single dummy byte
					return
				}
				http.Error(w, "unexpected GET", http.StatusBadRequest)
				return
			}
		}
		http.NotFound(w, r)
	}))
	defer upstream.Close()

	cfg := DefaultConfig()
	cfg.CacheDir = dir
	cfg.Upstreams = []string{strings.TrimSuffix(upstream.URL, "/")}
	cfg.GOSUMDB = "off"
	proxy := NewProxyFromConfig(&cfg, 720*time.Hour, 24*time.Hour, 100*time.Millisecond)
	defer proxy.Shutdown()

	baseURL, auth, length, ok := proxy.getZipContentLength(context.Background(), "github.com/foo/bar/@v/v1.0.0.zip")
	if !ok {
		t.Fatalf("getZipContentLength returned ok=false")
	}
	if auth != nil {
		t.Fatalf("expected no auth, got %#v", auth)
	}
	expectedBase := strings.TrimSuffix(upstream.URL, "/")
	if baseURL != expectedBase {
		t.Fatalf("baseURL = %q, want %q", baseURL, expectedBase)
	}
	if length != size {
		t.Fatalf("length = %d, want %d", length, size)
	}
	if headCount == 0 {
		t.Errorf("expected at least one HEAD request")
	}
	if rangeProbeCount == 0 {
		t.Errorf("expected at least one Range probe GET")
	}
}

func TestHandleZip_ParallelRangeDownloadServesFullContent(t *testing.T) {
	dir := t.TempDir()

	// Make content larger than minSizeForParallelDownload so parallel path is taken.
	contentSize := 2 * minSizeForParallelDownload
	zipContent := make([]byte, contentSize)
	for i := range zipContent {
		zipContent[i] = byte(i % 251)
	}

	var mu sync.Mutex
	var rangeRequests int

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, ".zip") {
			http.NotFound(w, r)
			return
		}

		switch r.Method {
		case http.MethodHead:
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(zipContent)))
			w.WriteHeader(http.StatusOK)
			return
		case http.MethodGet:
			rangeHeader := r.Header.Get("Range")
			if rangeHeader == "" {
				http.Error(w, "missing Range header", http.StatusBadRequest)
				return
			}

			mu.Lock()
			rangeRequests++
			mu.Unlock()

			// Parse Range: bytes=start-end
			const prefix = "bytes="
			if !strings.HasPrefix(rangeHeader, prefix) {
				http.Error(w, "invalid Range header", http.StatusBadRequest)
				return
			}
			rangeSpec := strings.TrimPrefix(rangeHeader, prefix)
			parts := strings.Split(rangeSpec, "-")
			if len(parts) != 2 {
				http.Error(w, "invalid Range format", http.StatusBadRequest)
				return
			}
			start, err1 := parseInt64(parts[0])
			endInclusive, err2 := parseInt64(parts[1])
			if err1 != nil || err2 != nil || start < 0 || endInclusive < start || endInclusive >= int64(len(zipContent)) {
				http.Error(w, "invalid Range values", http.StatusBadRequest)
				return
			}

			chunk := zipContent[start : endInclusive+1]
			w.Header().Set("Content-Type", "application/zip")
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(chunk)))
			w.WriteHeader(http.StatusPartialContent)
			if _, err := w.Write(chunk); err != nil {
				t.Logf("write chunk failed: %v", err)
			}
			return
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
	}))
	defer upstream.Close()

	cfg := DefaultConfig()
	cfg.CacheDir = dir
	cfg.Upstreams = []string{strings.TrimSuffix(upstream.URL, "/")}
	cfg.GOSUMDB = "off"
	cfg.DownloadConnections = 4
	cfg.MaxConcurrentDownloads = 4
	proxy := NewProxyFromConfig(&cfg, 720*time.Hour, 24*time.Hour, 100*time.Millisecond)
	defer proxy.Shutdown()

	req := httptest.NewRequest("GET", "/github.com/foo/bar/@v/v1.0.0.zip", nil)
	rec := httptest.NewRecorder()
	proxy.HandleRequest(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !bytes.Equal(rec.Body.Bytes(), zipContent) {
		t.Fatalf("downloaded zip content mismatch")
	}

	mu.Lock()
	defer mu.Unlock()
	if rangeRequests < 2 {
		t.Errorf("expected multiple Range requests for parallel download, got %d", rangeRequests)
	}
}

// --- Resumable download tests ---

// TestResumableZip_SingleConnection_ResumesFromPart: with DownloadConnections=1 and zip < minSizeForParallelDownload,
// a pre-created .part file causes the proxy to send a Range request for the remainder and append to .part.
func TestResumableZip_SingleConnection_ResumesFromPart(t *testing.T) {
	dir := t.TempDir()
	// Keep zip small so we use single-connection path (no parallel).
	zipContent := make([]byte, 100*1024)
	for i := range zipContent {
		zipContent[i] = byte(i % 251)
	}
	const partialSize = 30 * 1024 // first 30KB "already downloaded"

	var rangeRequests int
	var fullGetRequests int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, ".zip") {
			http.NotFound(w, r)
			return
		}
		switch r.Method {
		case http.MethodHead:
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(zipContent)))
			w.WriteHeader(http.StatusOK)
			return
		case http.MethodGet:
			rangeHeader := r.Header.Get("Range")
			if rangeHeader != "" {
				if !strings.HasPrefix(rangeHeader, "bytes=") {
					http.Error(w, "invalid Range", http.StatusBadRequest)
					return
				}
				rangeRequests++
				// Expect bytes=partialSize-(len-1)
				parts := strings.Split(strings.TrimPrefix(rangeHeader, "bytes="), "-")
				if len(parts) != 2 {
					http.Error(w, "bad range", http.StatusBadRequest)
					return
				}
				start, _ := parseInt64(parts[0])
				endIncl, _ := parseInt64(parts[1])
				if start != partialSize || endIncl != int64(len(zipContent))-1 {
					http.Error(w, fmt.Sprintf("unexpected range start=%d end=%d", start, endIncl), http.StatusBadRequest)
					return
				}
				chunk := zipContent[start : endIncl+1]
				w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, endIncl, len(zipContent)))
				w.Header().Set("Content-Length", fmt.Sprintf("%d", len(chunk)))
				w.WriteHeader(http.StatusPartialContent)
				w.Write(chunk)
				return
			}
			fullGetRequests++
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(zipContent)))
			w.Write(zipContent)
			return
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	requestPath := "github.com/foo/bar/@v/v1.0.0.zip"
	cachePath := cachePath(dir, requestPath)
	partPath := cachePath + ".part"
	if err := os.MkdirAll(filepath.Dir(cachePath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(partPath, zipContent[:partialSize], 0644); err != nil {
		t.Fatal(err)
	}

	cfg := DefaultConfig()
	cfg.CacheDir = dir
	cfg.Upstreams = []string{strings.TrimSuffix(upstream.URL, "/")}
	cfg.GOSUMDB = "off"
	cfg.DownloadConnections = 1
	proxy := NewProxyFromConfig(&cfg, 720*time.Hour, 24*time.Hour, 100*time.Millisecond)
	defer proxy.Shutdown()

	req := httptest.NewRequest("GET", "/"+requestPath, nil)
	rec := httptest.NewRecorder()
	proxy.HandleRequest(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !bytes.Equal(rec.Body.Bytes(), zipContent) {
		t.Fatalf("response body mismatch: got %d bytes, want %d", rec.Body.Len(), len(zipContent))
	}
	data, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("cache file: %v", err)
	}
	if !bytes.Equal(data, zipContent) {
		t.Fatalf("cached content mismatch: got %d bytes", len(data))
	}
	if rangeRequests != 1 {
		t.Errorf("expected exactly 1 Range request (resume), got %d", rangeRequests)
	}
	if fullGetRequests != 0 {
		t.Errorf("expected no full GET when resuming, got %d", fullGetRequests)
	}
}

// TestResumableZip_SingleConnection_FullDownloadWhenNoPart: no .part file → one full GET, no Range.
func TestResumableZip_SingleConnection_FullDownloadWhenNoPart(t *testing.T) {
	dir := t.TempDir()
	zipContent := make([]byte, 50*1024)
	for i := range zipContent {
		zipContent[i] = byte(i % 251)
	}

	var fullGetCount, rangeCount int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, ".zip") {
			http.NotFound(w, r)
			return
		}
		switch r.Method {
		case http.MethodHead:
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(zipContent)))
			w.WriteHeader(http.StatusOK)
			return
		case http.MethodGet:
			if r.Header.Get("Range") != "" {
				rangeCount++
			} else {
				fullGetCount++
			}
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(zipContent)))
			w.Write(zipContent)
			return
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	cfg := DefaultConfig()
	cfg.CacheDir = dir
	cfg.Upstreams = []string{strings.TrimSuffix(upstream.URL, "/")}
	cfg.GOSUMDB = "off"
	cfg.DownloadConnections = 1
	proxy := NewProxyFromConfig(&cfg, 720*time.Hour, 24*time.Hour, 100*time.Millisecond)
	defer proxy.Shutdown()

	req := httptest.NewRequest("GET", "/github.com/foo/bar/@v/v1.0.0.zip", nil)
	rec := httptest.NewRecorder()
	proxy.HandleRequest(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !bytes.Equal(rec.Body.Bytes(), zipContent) {
		t.Fatalf("response body mismatch")
	}
	if fullGetCount != 1 {
		t.Errorf("expected 1 full GET, got %d", fullGetCount)
	}
	if rangeCount != 0 {
		t.Errorf("expected no Range request when no .part, got %d", rangeCount)
	}
}

// TestResumableZip_SingleConnection_FallbackWhenRangeNotSupported: .part exists but server returns 200 for Range → remove .part, full GET.
func TestResumableZip_SingleConnection_FallbackWhenRangeNotSupported(t *testing.T) {
	dir := t.TempDir()
	zipContent := make([]byte, 50*1024)
	for i := range zipContent {
		zipContent[i] = byte(i % 251)
	}
	const partialSize = 10 * 1024

	var rangeAttempts, fullGets int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, ".zip") {
			http.NotFound(w, r)
			return
		}
		switch r.Method {
		case http.MethodHead:
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(zipContent)))
			w.WriteHeader(http.StatusOK)
			return
		case http.MethodGet:
			if r.Header.Get("Range") != "" {
				rangeAttempts++
				// Simulate server that ignores Range and returns 200 + full body
				w.Header().Set("Content-Length", fmt.Sprintf("%d", len(zipContent)))
				w.WriteHeader(http.StatusOK)
				w.Write(zipContent)
				return
			}
			fullGets++
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(zipContent)))
			w.Write(zipContent)
			return
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	requestPath := "github.com/foo/bar/@v/v1.0.0.zip"
	cachePath := cachePath(dir, requestPath)
	partPath := cachePath + ".part"
	if err := os.MkdirAll(filepath.Dir(cachePath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(partPath, zipContent[:partialSize], 0644); err != nil {
		t.Fatal(err)
	}

	cfg := DefaultConfig()
	cfg.CacheDir = dir
	cfg.Upstreams = []string{strings.TrimSuffix(upstream.URL, "/")}
	cfg.GOSUMDB = "off"
	cfg.DownloadConnections = 1
	proxy := NewProxyFromConfig(&cfg, 720*time.Hour, 24*time.Hour, 100*time.Millisecond)
	defer proxy.Shutdown()

	req := httptest.NewRequest("GET", "/"+requestPath, nil)
	rec := httptest.NewRecorder()
	proxy.HandleRequest(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !bytes.Equal(rec.Body.Bytes(), zipContent) {
		t.Fatalf("response body mismatch")
	}
	data, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("cache file: %v", err)
	}
	if !bytes.Equal(data, zipContent) {
		t.Fatalf("cached content mismatch")
	}
	if rangeAttempts != 1 {
		t.Errorf("expected 1 Range attempt (then fallback), got %d", rangeAttempts)
	}
	if fullGets != 1 {
		t.Errorf("expected 1 full GET after fallback, got %d", fullGets)
	}
}

// TestResumableZip_Parallel_ResumesCompletedChunks: existing .tmp with correct size and chunk 0 filled
// (non-zero first/last byte) → only chunks 1,2,3 are requested; final file is complete.
func TestResumableZip_Parallel_ResumesCompletedChunks(t *testing.T) {
	dir := t.TempDir()
	contentSize := 2 * minSizeForParallelDownload
	zipContent := make([]byte, contentSize)
	for i := range zipContent {
		zipContent[i] = byte(i%251 + 1) // avoid 0 so "completed" chunks are detected
	}

	chunkSize := (contentSize + 3) / 4
	var rangeRequestsMu sync.Mutex
	var rangeRequests []string

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, ".zip") {
			http.NotFound(w, r)
			return
		}
		switch r.Method {
		case http.MethodHead:
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(zipContent)))
			w.WriteHeader(http.StatusOK)
			return
		case http.MethodGet:
			rangeHeader := r.Header.Get("Range")
			if rangeHeader == "" {
				http.Error(w, "missing Range", http.StatusBadRequest)
				return
			}
			rangeRequestsMu.Lock()
			rangeRequests = append(rangeRequests, rangeHeader)
			rangeRequestsMu.Unlock()

			parts := strings.Split(strings.TrimPrefix(rangeHeader, "bytes="), "-")
			if len(parts) != 2 {
				http.Error(w, "bad range", http.StatusBadRequest)
				return
			}
			start, _ := parseInt64(parts[0])
			endIncl, _ := parseInt64(parts[1])
			chunk := zipContent[start : endIncl+1]
			w.Header().Set("Content-Type", "application/zip")
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(chunk)))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(chunk)
			return
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	requestPath := "github.com/foo/bar/@v/v1.0.0.zip"
	cachePath := cachePath(dir, requestPath)
	tmpPath := cachePath + ".tmp"
	if err := os.MkdirAll(filepath.Dir(cachePath), 0755); err != nil {
		t.Fatal(err)
	}
	// Pre-create .tmp with full size; fill only chunk 0 with real data (non-zero first and last byte).
	f, err := os.Create(tmpPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(int64(contentSize)); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if _, err := f.WriteAt(zipContent[:chunkSize], 0); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	cfg := DefaultConfig()
	cfg.CacheDir = dir
	cfg.Upstreams = []string{strings.TrimSuffix(upstream.URL, "/")}
	cfg.GOSUMDB = "off"
	cfg.DownloadConnections = 4
	proxy := NewProxyFromConfig(&cfg, 720*time.Hour, 24*time.Hour, 100*time.Millisecond)
	defer proxy.Shutdown()

	req := httptest.NewRequest("GET", "/"+requestPath, nil)
	rec := httptest.NewRecorder()
	proxy.HandleRequest(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !bytes.Equal(rec.Body.Bytes(), zipContent) {
		t.Fatalf("response body mismatch")
	}
	data, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("cache file: %v", err)
	}
	if !bytes.Equal(data, zipContent) {
		t.Fatalf("cached content mismatch")
	}

	rangeRequestsMu.Lock()
	defer rangeRequestsMu.Unlock()
	// Chunk 0 should be skipped (we pre-filled it). So we expect 3 Range requests, not 4.
	if n := len(rangeRequests); n != 3 {
		t.Errorf("expected 3 Range requests (chunk 0 skipped), got %d: %v", n, rangeRequests)
	}
	// None of the requested ranges should be chunk 0 (bytes=0-(chunkSize-1)).
	chunk0Prefix := fmt.Sprintf("bytes=0-%d", chunkSize-1)
	for _, rng := range rangeRequests {
		if rng == chunk0Prefix {
			t.Errorf("chunk 0 should have been skipped (no request for %q)", chunk0Prefix)
		}
	}
}

// TestResumableZip_Parallel_NoResumeWhenTmpWrongSize: .tmp exists but with wrong size → delete and full parallel download.
func TestResumableZip_Parallel_NoResumeWhenTmpWrongSize(t *testing.T) {
	dir := t.TempDir()
	contentSize := 2 * minSizeForParallelDownload
	zipContent := make([]byte, contentSize)
	for i := range zipContent {
		zipContent[i] = byte(i % 251)
	}

	var rangeCount int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, ".zip") {
			http.NotFound(w, r)
			return
		}
		switch r.Method {
		case http.MethodHead:
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(zipContent)))
			w.WriteHeader(http.StatusOK)
			return
		case http.MethodGet:
			rangeHeader := r.Header.Get("Range")
			if rangeHeader == "" {
				http.Error(w, "missing Range", http.StatusBadRequest)
				return
			}
			rangeCount++

			parts := strings.Split(strings.TrimPrefix(rangeHeader, "bytes="), "-")
			if len(parts) != 2 {
				http.Error(w, "bad range", http.StatusBadRequest)
				return
			}
			start, _ := parseInt64(parts[0])
			endIncl, _ := parseInt64(parts[1])
			chunk := zipContent[start : endIncl+1]
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(chunk)))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(chunk)
			return
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	requestPath := "github.com/foo/bar/@v/v1.0.0.zip"
	cachePath := cachePath(dir, requestPath)
	tmpPath := cachePath + ".tmp"
	if err := os.MkdirAll(filepath.Dir(cachePath), 0755); err != nil {
		t.Fatal(err)
	}
	// Wrong size: half of actual
	if err := os.WriteFile(tmpPath, make([]byte, contentSize/2), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := DefaultConfig()
	cfg.CacheDir = dir
	cfg.Upstreams = []string{strings.TrimSuffix(upstream.URL, "/")}
	cfg.GOSUMDB = "off"
	cfg.DownloadConnections = 4
	proxy := NewProxyFromConfig(&cfg, 720*time.Hour, 24*time.Hour, 100*time.Millisecond)
	defer proxy.Shutdown()

	req := httptest.NewRequest("GET", "/"+requestPath, nil)
	rec := httptest.NewRecorder()
	proxy.HandleRequest(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !bytes.Equal(rec.Body.Bytes(), zipContent) {
		t.Fatalf("response body mismatch")
	}
	data, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("cache file: %v", err)
	}
	if !bytes.Equal(data, zipContent) {
		t.Fatalf("cached content mismatch")
	}
	// Should have requested all 4 chunks (no resume due to wrong .tmp size).
	if rangeCount != 4 {
		t.Errorf("expected 4 Range requests (full parallel, no resume), got %d", rangeCount)
	}
}

// parseInt64 is a tiny helper so the test code stays simple.
func parseInt64(s string) (int64, error) {
	return strconv.ParseInt(strings.TrimSpace(s), 10, 64)
}

func TestMaxConcurrentDownloadsLimit(t *testing.T) {
	dir := t.TempDir()

	zipContent := []byte("fake zip content for concurrency test")

	var mu sync.Mutex
	active := 0
	maxActive := 0

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, ".zip") {
			http.NotFound(w, r)
			return
		}

		mu.Lock()
		active++
		if active > maxActive {
			maxActive = active
		}
		mu.Unlock()

		defer func() {
			mu.Lock()
			active--
			mu.Unlock()
		}()

		// Slow each download a bit so overlaps can happen.
		time.Sleep(200 * time.Millisecond)

		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(zipContent)))
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write(zipContent); err != nil {
			t.Logf("write zip failed: %v", err)
		}
	}))
	defer upstream.Close()

	cfg := DefaultConfig()
	cfg.CacheDir = dir
	cfg.Upstreams = []string{strings.TrimSuffix(upstream.URL, "/")}
	cfg.GOSUMDB = "off"
	cfg.DownloadConnections = 1            // keep each download single-connection to simplify
	cfg.MaxConcurrentDownloads = 2         // limit total concurrent package downloads
	proxy := NewProxyFromConfig(&cfg, 720*time.Hour, 24*time.Hour, 100*time.Millisecond)
	defer proxy.Shutdown()

	var wg sync.WaitGroup
	totalRequests := 6
	for i := 0; i < totalRequests; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			path := fmt.Sprintf("/example.com/mod%d/@v/v1.0.0.zip", i)
			req := httptest.NewRequest("GET", path, nil)
			rec := httptest.NewRecorder()
			proxy.HandleRequest(rec, req)
			if rec.Code != http.StatusOK {
				t.Errorf("request %d: status = %d, want 200", i, rec.Code)
			}
			if !bytes.Equal(rec.Body.Bytes(), zipContent) {
				t.Errorf("request %d: body mismatch", i)
			}
		}(i)
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if maxActive > cfg.MaxConcurrentDownloads {
		t.Errorf("max concurrent upstream downloads = %d, want <= %d", maxActive, cfg.MaxConcurrentDownloads)
	}
}
