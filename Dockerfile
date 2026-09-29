# syntax=docker/dockerfile:1

# Keep in step with the go directive in go.mod; CI passes it in from there.
ARG GO_VERSION=1.27

# Build from the repo source rather than `go install module@version`: the
# latter ignores go.mod's replace directive and would pull upstream appie-go
# instead of the celerex fork this server depends on.
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION} AS build
WORKDIR /src
# GOTOOLCHAIN=local: fail loudly if go.mod outgrows the image instead of
# silently downloading another toolchain mid-build.
ENV CGO_ENABLED=0 GOTOOLCHAIN=local

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .
ARG TARGETOS TARGETARCH
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/ah-mcp . \
 && mkdir -p /out/data/ah-mcp

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ah-mcp /ah-mcp
# Owned by nonroot so a fresh named volume on /data is writable. A bind mount
# replaces this, so the host directory must be owned by 65532:65532.
COPY --from=build --chown=65532:65532 /out/data /data

# AH_REMOTE: there is no browser to open inside a container, and the login
#   proxy on 9876 must accept connections from outside the container.
# AH_MCP_BIND/AH_MCP_PORT: listen on 0.0.0.0:8080; startup refuses this
#   without AH_MCP_TOKEN or OAuth (AH_MCP_OAUTH_ISSUER).
ENV AH_TOKENS_PATH=/data/ah-mcp/tokens.json \
    AH_MCP_BIND=0.0.0.0 \
    AH_MCP_PORT=8080 \
    AH_REMOTE=true

EXPOSE 8080 9876
USER nonroot:nonroot

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["/ah-mcp", "--healthcheck"]

ENTRYPOINT ["/ah-mcp"]
CMD ["--transport", "streamable-http"]
