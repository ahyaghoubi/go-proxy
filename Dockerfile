# Build stage
FROM golang:1.21-alpine AS builder

WORKDIR /app

# Copy go mod files
COPY go.mod ./
# Copy go.sum if it exists (optional)
COPY go.sum* ./

# Download dependencies
RUN go mod download

# Copy source code
COPY . .

# Build the application
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o goproxy .

# Runtime stage
FROM alpine:latest

# Install CA certificates and wget for healthcheck
RUN apk --no-cache add ca-certificates wget

WORKDIR /app

# Copy binary from builder
COPY --from=builder /app/goproxy .

# Expose port
EXPOSE 12345

# Create volume for cache directory
VOLUME ["/app/cache"]

# Run the binary
CMD ["./goproxy"]

