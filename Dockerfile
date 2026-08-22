# syntax=docker/dockerfile:1
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/hop .
# a /data skeleton owned by nonroot: a fresh named volume inherits its ownership
RUN mkdir -p /skel/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/hop /hop
COPY --from=build --chown=nonroot:nonroot /skel/data /data
USER nonroot:nonroot
EXPOSE 8090
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 CMD ["/hop", "healthcheck"]
ENTRYPOINT ["/hop"]
