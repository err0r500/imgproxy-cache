FROM golang:1.24-alpine AS builder
RUN apk add --no-cache git

WORKDIR /app

# Copy Go module files
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY start_processes.sh main.go ./

# Build the proxy
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags "-s -w" -o proxy .

# Final stage using official imgproxy
FROM ghcr.io/imgproxy/imgproxy:v3.30

# Copy our proxy binary
COPY --from=builder /app/proxy /usr/local/bin/
COPY start_processes.sh /usr/local/bin/

EXPOSE 8080

# Start proxy (which will start the cache proxy & imgproxy)
CMD ["/usr/local/bin/start_processes.sh"]
