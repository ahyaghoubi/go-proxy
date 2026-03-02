package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
	pr := newProgressReader(r, int64(len(content)), "test/path")
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
	// Use large enough payload and slow reader so progress ticker (500ms) fires at least once
	zipContent := bytes.Repeat([]byte("x"), 100*1024) // 100KB
	sr := &slowReader{data: zipContent, delay: 300 * time.Millisecond, chunk: 32 * 1024} // ~1s total, 2+ progress ticks

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".zip") {
			w.Header().Set("Content-Type", "application/zip")
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(zipContent)))
			io.Copy(w, sr)
			return
		}
		http.NotFound(w, r)
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

	// Progress output must contain [DOWNLOAD], path, and progress info
	if !strings.Contains(output, "[DOWNLOAD]") {
		t.Error("output should contain [DOWNLOAD]")
	}
	if !strings.Contains(output, "github.com/foo/bar/@v/v1.0.0.zip") {
		t.Error("output should contain module path")
	}
	// Progress info: percent, speed, remaining
	if !strings.Contains(output, "%") {
		t.Error("output should contain percent")
	}
	if !strings.Contains(output, "/s") {
		t.Error("output should contain speed (/s)")
	}
	if !strings.Contains(output, "left") || !strings.Contains(output, "ETA") {
		t.Error("output should contain remaining and ETA")
	}
}

func TestDownloadProgressUpdatesLive(t *testing.T) {
	// Slow download (600ms+) so at least 2 progress updates (ticker every 500ms)
	zipContent := bytes.Repeat([]byte("a"), 200*1024) // 200KB
	sr := &slowReader{data: zipContent, delay: 300 * time.Millisecond, chunk: 32 * 1024} // ~2s total

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".zip") {
			w.Header().Set("Content-Type", "application/zip")
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(zipContent)))
			io.Copy(w, sr)
			return
		}
		http.NotFound(w, r)
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
	lines := strings.Split(output, "\n")

	// In non-TTY (pipe), each progress update is a new line; we expect at least 2 progress lines
	var progressLines int
	for _, line := range lines {
		if strings.Contains(line, "[DOWNLOAD]") && strings.Contains(line, "github.com/bar/baz") {
			progressLines++
		}
	}
	if progressLines < 2 {
		t.Errorf("expected at least 2 progress updates (live updates), got %d", progressLines)
	}
}

func TestDownloadProgressStaysAtBottomWhenTTY(t *testing.T) {
	// When stderr is TTY, output uses \r (carriage return) for in-place updates
	// In tests stderr is a pipe (non-TTY), so we get \n per line instead
	// Use slow download so progress ticker (500ms) fires at least once
	zipContent := bytes.Repeat([]byte("x"), 50*1024)
	sr := &slowReader{data: zipContent, delay: 350 * time.Millisecond, chunk: 16 * 1024}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".zip") {
			w.Header().Set("Content-Type", "application/zip")
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(zipContent)))
			io.Copy(w, sr)
			return
		}
		http.NotFound(w, r)
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
	// Progress output should exist (format varies by TTY vs non-TTY)
	if !strings.Contains(output, "[DOWNLOAD]") {
		t.Error("download progress should be written to stderr")
	}
}
