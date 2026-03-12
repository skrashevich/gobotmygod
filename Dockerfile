# syntax=docker/dockerfile:1.4

FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

ARG TARGETOS TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -ldflags="-s -w" -o /botmux .

FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata

COPY --from=builder /botmux /usr/local/bin/botmux

HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD wget -qO- http://localhost:8080/api/health || exit 1

VOLUME /data
EXPOSE 8080

ENTRYPOINT ["botmux"]
CMD ["-addr", ":8080", "-db", "/data/botdata.db"]
