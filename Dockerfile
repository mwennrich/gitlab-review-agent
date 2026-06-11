# --- Stage 1: Go Build ---
FROM golang:1.26 AS go-builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY agent.go .
COPY cmd/shellmcp ./cmd/shellmcp
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/agent ./agent.go && \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/shellmcp ./cmd/shellmcp

# --- Stage 2: Runtime with MCP servers ---
FROM node:slim

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    curl \
    git \
    ripgrep \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

# Prefetch MCP server packages into npm cache (faster first run)
RUN npm cache add @modelcontextprotocol/server-filesystem

COPY --from=go-builder /out/agent /app/agent
COPY --from=go-builder /out/shellmcp /app/shellmcp

CMD ["/app/agent"]
