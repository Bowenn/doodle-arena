# syntax=docker/dockerfile:1
# The build context is the repository root, so the build can use the generated contracts in
# proto/gen/go and the shared Go packages in libs/go (go.mod replaces both with ../../ paths).
# Dockerfile.dockerignore, next to this file, sends only those and the service to the builder.
FROM golang:1.27.2-trixie AS build
WORKDIR /src/services/game-server
COPY proto/gen/go/go.mod proto/gen/go/go.sum /src/proto/gen/go/
COPY libs/go/go.mod libs/go/go.sum /src/libs/go/
COPY services/game-server/go.mod services/game-server/go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY proto/gen/go /src/proto/gen/go
COPY libs/go /src/libs/go
COPY services/game-server ./
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/app ./cmd/game-server

# Distroless publishes no version tags, so the runtime is pinned by digest (the nonroot
# multi-arch index as of 2026-10-17); bump it deliberately, in its own commit.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3
COPY --from=build /out/app /app
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/app"]
