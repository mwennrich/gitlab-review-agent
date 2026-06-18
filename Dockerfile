# --- Stage 1: Go Build
FROM golang:1.26 AS go-builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd/shellmcp ./cmd/shellmcp
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/shellmcp ./cmd/shellmcp
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go install github.com/mark3labs/mcp-filesystem-server@latest
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go install github.com/cnosuke/mcp-fetch@latest


# --- Stage 2: Build agent ---
FROM golang:1.26 AS agent-builder
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download
COPY agent.go ./agent.go
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/agent ./agent.go


# --- Stage 3: Runtime with MCP servers ---
FROM debian:trixie-slim

RUN apt-get update && apt-get install -y \
    ca-certificates \
    curl \
    git \
    ripgrep \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY --from=go-builder /out/shellmcp /app/shellmcp
COPY --from=go-builder /go/bin/mcp-filesystem-server /app/mcp-filesystem-server
COPY --from=go-builder /go/bin/mcp-fetch /app/mcp-fetch

COPY --from=agent-builder /out/agent /app/agent

CMD ["/app/agent"]
