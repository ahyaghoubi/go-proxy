# Go Module Proxy

A production-ready local Go module proxy server that caches downloaded packages and modules on disk. This proxy implements the [Go module proxy protocol](https://go.dev/ref/mod#goproxy-protocol) and serves as a caching layer between your Go toolchain and upstream module proxies.

## Features

- ✅ Full Go module proxy protocol implementation
- ✅ Disk-based caching for all module artifacts
- ✅ Thread-safe cache operations
- ✅ Atomic file writes to prevent corruption
- ✅ Graceful shutdown handling
- ✅ Configurable upstream proxy
- ✅ HTTP client with proper timeouts and connection pooling
- ✅ Content-Length headers for HTTP compliance
- ✅ Comprehensive logging
- ✅ Environment variable and CLI flag support

## Architecture

```
┌─────────────┐
│ Go Toolchain│
└──────┬──────┘
       │ HTTP Request
       ▼
┌──────────────────┐
│  Proxy Server    │
│   (Port 12345)   │
└──────┬──────┬────┘
       │      │
       │      │ Cache Miss
       │      ▼
       │  ┌──────────────┐
       │  │ Upstream     │
       │  │ Proxy        │
       │  └──────┬───────┘
       │         │
       │         │ Fetch
       │         ▼
       │  ┌──────────────┐
       │  │ Cache Disk   │
       │  └──────────────┘
       │
       │ Cache Hit
       ▼
┌──────────────┐
│ Cache Disk   │
└──────────────┘
```

## Installation

### Prerequisites

- Go 1.21 or later
- Git (for cloning)

### Build from Source

```bash
git clone <repository-url>
cd goproxy
go mod download
go build -o goproxy
```

## Usage

### Basic Usage

Start the proxy server with default settings:

```bash
./goproxy
```

This will:
- Listen on port `12345`
- Use `./cache` as the cache directory
- Use `https://proxy.golang.org` as the upstream proxy

### Configuration Options

#### Command-Line Flags

```bash
./goproxy -port 3000 -cache /path/to/cache -upstream https://proxy.golang.org
```

Options:
- `-port`: Port to listen on (default: `12345`)
- `-cache`: Cache directory path (default: `./cache`)
- `-upstream`: Upstream proxy URL (default: `https://proxy.golang.org`)

#### Environment Variables

Environment variables override command-line flags:

```bash
export PORT=3000
export CACHE_DIR=/path/to/cache
export UPSTREAM_PROXY=https://proxy.golang.org
./goproxy
```

### Configure Go to Use the Proxy

Set the `GOPROXY` environment variable:

```bash
export GOPROXY=http://localhost:12345,direct
```

Or for a specific Go command:

```bash
GOPROXY=http://localhost:12345,direct go get example.com/module
```

The `,direct` fallback ensures that if the proxy doesn't have a module, Go will fetch it directly from the source.

### Proxy Support (For Sanctions/Geo-blocking)

The proxy supports HTTP, HTTPS, and SOCKS5 proxies to bypass restrictions and work in restricted environments.

#### Command-Line Flag

```bash
# HTTP/HTTPS proxy
./goproxy -proxy http://proxy-server:8080

# SOCKS5 proxy
./goproxy -proxy socks5://socks5-server:1080

# SOCKS5 with hostname resolution on proxy side
./goproxy -proxy socks5h://socks5-server:1080
```

#### Environment Variables

The proxy checks environment variables in this order:
1. `HTTP_PROXY` - HTTP proxy URL
2. `HTTPS_PROXY` - HTTPS proxy URL  
3. `SOCKS5_PROXY` - SOCKS5 proxy URL

```bash
# HTTP proxy
export HTTP_PROXY=http://proxy-server:8080
./goproxy

# SOCKS5 proxy
export SOCKS5_PROXY=socks5://socks5-server:1080
./goproxy
```

**Priority:** Command-line flag (`-proxy`) takes precedence over environment variables.

#### Alternative Upstream Proxies

If `proxy.golang.org` is blocked, try alternative mirrors:

```bash
# Chinese mirror (may be accessible from restricted regions)
./goproxy -upstream https://goproxy.cn

# Or use environment variable
export UPSTREAM_PROXY=https://goproxy.cn
./goproxy
```

#### Supported Proxy Schemes

- `http://` - HTTP proxy
- `https://` - HTTPS proxy (treated as HTTP)
- `socks5://` - SOCKS5 proxy (DNS resolution on client)
- `socks5h://` - SOCKS5 proxy (DNS resolution on proxy)

#### Example: Using Proxy with Alternative Upstream

```bash
./goproxy -proxy socks5://your-socks5-server:1080 -upstream https://goproxy.cn
```

### DNS Server Configuration

The proxy supports multiple DNS protocols for domain name resolution, useful when DNS is blocked or you want to use encrypted DNS queries.

#### Supported DNS Protocols

**Standard DNS (UDP)**
- Format: `8.8.8.8:53` or `udp://8.8.8.8:53`
- Default port: 53
- Examples:
  - Google DNS: `8.8.8.8:53`
  - Cloudflare DNS: `1.1.1.1:53`
  - Quad9: `9.9.9.9:53`

**DNS-over-HTTPS (DoH)**
- Format: `https://doh-server/dns-query`
- Uses JSON format (application/dns-json)
- Examples:
  - Cloudflare: `https://cloudflare-dns.com/dns-query`
  - Google: `https://dns.google/resolve`
  - Cloudflare IP: `https://1.1.1.1/dns-query`

**DNS-over-TLS (DoT)**
- Format: `tls://dns-server:853`
- Default port: 853
- Examples:
  - Cloudflare: `tls://1.1.1.1:853`
  - Google: `tls://8.8.8.8:853`
  - Quad9: `tls://9.9.9.9:853`

**DNS-over-QUIC (DoQ)**
- Format: `quic://dns-server:853`
- Default port: 853
- Examples:
  - AdGuard: `quic://dns.adguard.com:853`
  - Cloudflare: `quic://1.1.1.1:853`

#### Command-Line Flag

```bash
# Standard DNS (UDP)
./goproxy -dns 8.8.8.8:53

# DNS-over-HTTPS
./goproxy -dns https://cloudflare-dns.com/dns-query

# DNS-over-TLS
./goproxy -dns tls://1.1.1.1:853

# DNS-over-QUIC
./goproxy -dns quic://dns.adguard.com:853
```

#### Environment Variable

```bash
export DNS_SERVER=8.8.8.8:53
./goproxy

# Or use DoH
export DNS_SERVER=https://cloudflare-dns.com/dns-query
./goproxy
```

**Priority:** Command-line flag (`-dns`) takes precedence over environment variable.

#### Using DNS with Proxy

```bash
# Custom DNS + SOCKS5 proxy
./goproxy -dns 8.8.8.8:53 -proxy socks5://socks5-server:1080

# DoH + HTTP proxy + alternative upstream
./goproxy -dns https://cloudflare-dns.com/dns-query -proxy http://proxy:8080 -upstream https://goproxy.cn

# DoT + SOCKS5 proxy
./goproxy -dns tls://1.1.1.1:853 -proxy socks5://socks5-server:1080
```

#### Popular DNS Servers

**Standard DNS (UDP)**
- Google DNS: `8.8.8.8:53` or `8.8.4.4:53`
- Cloudflare DNS: `1.1.1.1:53` or `1.0.0.1:53`
- Quad9: `9.9.9.9:53`
- OpenDNS: `208.67.222.222:53` or `208.67.220.220:53`

**DNS-over-HTTPS (DoH)**
- Cloudflare: `https://cloudflare-dns.com/dns-query`
- Google: `https://dns.google/resolve`
- Quad9: `https://dns.quad9.net/dns-query`

**DNS-over-TLS (DoT)**
- Cloudflare: `tls://1.1.1.1:853`
- Google: `tls://8.8.8.8:853`
- Quad9: `tls://9.9.9.9:853`

**Note:** If port is not specified for UDP DNS, it defaults to `:53`. For TLS/QUIC, it defaults to `:853`.

### Docker Usage

#### Build the Image

```bash
docker build -t goproxy .
```

#### Run the Container

**Option 1: Using a bind mount (persist cache on host)**

```bash
docker run -d \
  --name goproxy \
  -p 12345:12345 \
  -v $(pwd)/cache:/app/cache \
  goproxy
```

**Option 2: Using a named Docker volume (managed by Docker)**

```bash
# Create a named volume
docker volume create goproxy-cache

# Run container with named volume
docker run -d \
  --name goproxy \
  -p 12345:12345 \
  -v goproxy-cache:/app/cache \
  goproxy
```

The cache directory (`/app/cache`) is declared as a volume in the Dockerfile, so Docker will automatically create a volume if none is specified.

#### With Custom Configuration

```bash
docker run -d \
  --name goproxy \
  -p 12345:12345 \
  -v $(pwd)/cache:/app/cache \
  -e UPSTREAM_PROXY=https://proxy.golang.org \
  goproxy
```

#### Docker with Proxy

```bash
# HTTP proxy
docker run -d \
  --name goproxy \
  -p 12345:12345 \
  -v $(pwd)/cache:/app/cache \
  -e HTTP_PROXY=http://proxy-server:8080 \
  -e UPSTREAM_PROXY=https://proxy.golang.org \
  goproxy

# SOCKS5 proxy
docker run -d \
  --name goproxy \
  -p 12345:12345 \
  -v $(pwd)/cache:/app/cache \
  -e SOCKS5_PROXY=socks5://socks5-server:1080 \
  -e UPSTREAM_PROXY=https://goproxy.cn \
  goproxy
```

#### Docker with DNS

```bash
# Standard DNS
docker run -d \
  --name goproxy \
  -p 12345:12345 \
  -v $(pwd)/cache:/app/cache \
  -e DNS_SERVER=8.8.8.8:53 \
  goproxy

# DNS-over-HTTPS
docker run -d \
  --name goproxy \
  -p 12345:12345 \
  -v $(pwd)/cache:/app/cache \
  -e DNS_SERVER=https://cloudflare-dns.com/dns-query \
  goproxy

# DNS + SOCKS5 proxy
docker run -d \
  --name goproxy \
  -p 12345:12345 \
  -v $(pwd)/cache:/app/cache \
  -e DNS_SERVER=8.8.8.8:53 \
  -e SOCKS5_PROXY=socks5://socks5-server:1080 \
  goproxy
```

### Docker Compose Usage

The easiest way to run the proxy is using Docker Compose:

#### Start the Service

```bash
docker-compose up -d
```

#### Stop the Service

```bash
docker-compose down
```

#### View Logs

```bash
docker-compose logs -f
```

#### Rebuild After Changes

```bash
docker-compose up -d --build
```

#### Using Bind Mount Instead of Named Volume

To use a bind mount (persist cache on host filesystem), edit `docker-compose.yml` and change:

```yaml
volumes:
  - goproxy-cache:/app/cache
```

to:

```yaml
volumes:
  - ./cache:/app/cache
```

The docker-compose.yml includes:
- Named volume for cache persistence
- Health check endpoint (`/health`)
- Automatic restart policy
- Port mapping (12345:12345)
- Environment variable support

#### Using Proxy and DNS with Docker Compose

To use a proxy and/or DNS with Docker Compose, edit `docker-compose.yml` and uncomment/modify the environment section:

```yaml
environment:
  # HTTP proxy
  HTTP_PROXY: http://proxy-server:8080
  # Or SOCKS5 proxy
  # SOCKS5_PROXY: socks5://socks5-server:1080
  # Alternative upstream (if proxy.golang.org is blocked)
  # UPSTREAM_PROXY: https://goproxy.cn
  # DNS configuration
  DNS_SERVER: 8.8.8.8:53
  # Or use DoH
  # DNS_SERVER: https://cloudflare-dns.com/dns-query
  # Or use DoT
  # DNS_SERVER: tls://1.1.1.1:853
```

Then restart the service:

```bash
docker-compose down
docker-compose up -d
```

## How It Works

The proxy implements the following Go module proxy protocol endpoints:

1. **`GET /<module>/@v/list`** - Lists available versions of a module
2. **`GET /<module>/@v/<version>.info`** - Returns version metadata (JSON)
3. **`GET /<module>/@v/<version>.mod`** - Returns the go.mod file for a version
4. **`GET /<module>/@v/<version>.zip`** - Returns the module zip file

### Request Flow

1. Client sends request to proxy
2. Proxy checks local cache (with read lock)
3. If cached:
   - Serve cached content immediately
   - Log cache hit
4. If not cached:
   - Fetch from upstream proxy
   - Validate response (JSON for .info endpoints)
   - Cache response atomically (using temp file + rename)
   - Serve response to client
   - Log cache miss

### Cache Structure

The cache directory structure mirrors the proxy URL structure:

```
cache/
├── github.com/
│   └── user/
│       └── repo/
│           └── @v/
│               ├── list
│               ├── v1.0.0.info
│               ├── v1.0.0.mod
│               └── v1.0.0.zip
```

## HTTP Client Configuration

The proxy uses a properly configured HTTP client with:

- **Total request timeout**: 30 seconds
- **Connection timeout**: 5 seconds
- **TLS handshake timeout**: 5 seconds
- **Response header timeout**: 10 seconds
- **Idle connection timeout**: 90 seconds
- **Max idle connections**: 100
- **Max idle connections per host**: 10

This ensures efficient connection reuse and prevents hanging requests.

## Graceful Shutdown

The server handles SIGINT and SIGTERM signals for graceful shutdown:

1. Receives shutdown signal
2. Stops accepting new requests
3. Waits up to 10 seconds for in-flight requests to complete
4. Shuts down cleanly

## Logging

The proxy logs:
- All incoming requests with client IP and path
- Cache hits and misses
- Errors with context
- Startup configuration
- Shutdown events

Example log output:
```
2024/01/01 12:00:00 Starting Go module proxy server
2024/01/01 12:00:00   Port: 12345
2024/01/01 12:00:00   Cache directory: ./cache
2024/01/01 12:00:00   Upstream proxy: https://proxy.golang.org
2024/01/01 12:00:00   Set GOPROXY=http://localhost:12345,direct
2024/01/01 12:00:05 [127.0.0.1] GET github.com/example/module/@v/list
2024/01/01 12:00:05 [CACHE MISS] github.com/example/module/@v/list
2024/01/01 12:00:06 [127.0.0.1] GET github.com/example/module/@v/v1.0.0.info
2024/01/01 12:00:06 [CACHE HIT] github.com/example/module/@v/v1.0.0.info
```

## Development

### Building

```bash
go build -o goproxy
```

### Running Tests

```bash
go test ./...
```

### Generating go.sum

After adding dependencies:

```bash
go mod tidy
```

## Troubleshooting

### Port Already in Use

If port 12345 is already in use, specify a different port:

```bash
./goproxy -port 8080
```

### Permission Denied for Cache Directory

Ensure the cache directory is writable:

```bash
chmod 755 ./cache
```

Or specify a different cache directory:

```bash
./goproxy -cache /tmp/goproxy-cache
```

### Upstream Proxy Errors

If you see upstream proxy errors, check:

1. Network connectivity
2. Upstream proxy URL is correct
3. Firewall/proxy settings
4. SSL certificate issues

### Cache Corruption

If you suspect cache corruption, delete the cache directory:

```bash
rm -rf ./cache
```

The proxy will recreate it on startup.

## Security Considerations

- The proxy does not implement authentication - consider placing it behind a reverse proxy with authentication if needed
- Cache files are stored with 0644 permissions (readable by all)
- Consider implementing cache size limits and cleanup policies for production use
- Monitor logs for unusual activity

## License

MIT

# go-proxy
