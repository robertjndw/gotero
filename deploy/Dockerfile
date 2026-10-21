# syntax=docker/dockerfile:1.7

FROM --platform=$BUILDPLATFORM node:22-alpine AS web
WORKDIR /src/web
RUN corepack enable
COPY web/package.json web/pnpm-lock.yaml ./
RUN --mount=type=cache,target=/root/.local/share/pnpm/store pnpm install --frozen-lockfile
COPY web/ ./
RUN mkdir -p ../internal/webui/dist && pnpm build

FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
WORKDIR /src
COPY go.work go.work.sum* ./
COPY go.mod go.sum ./
COPY zotero/go.mod zotero/go.sum* zotero/
COPY zoteroclient/go.mod zoteroclient/go.sum* zoteroclient/
COPY storage/go.mod storage/go.sum* storage/
COPY conformance/go.mod conformance/go.sum* conformance/
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
COPY --from=web /src/internal/webui/dist internal/webui/dist
ARG TARGETOS TARGETARCH VERSION=dev COMMIT=unknown
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
      -ldflags "-s -w -X github.com/robertjndw/gotero/internal/version.Version=${VERSION} -X github.com/robertjndw/gotero/internal/version.Commit=${COMMIT}" \
      -o /out/gotero ./cmd/gotero
# distroless has no shell to chown at runtime. Docker copies image content
# into a fresh named volume, ownership included, so shipping /data owned by
# nonroot makes the default volume writable.
RUN mkdir -p /out/data/files

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/gotero /usr/local/bin/gotero
COPY --from=build --chown=65532:65532 /out/data /data
# Default file storage location for the local backend; mount a volume here.
VOLUME /data
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/gotero"]
CMD ["serve"]
