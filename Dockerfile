# syntax=docker/dockerfile:1
FROM golang:1.27-alpine AS builder
RUN apk add --no-cache git
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
# .git 提供版本（git commit）；GIT_VERSION 允许外部覆盖
COPY .git ./.git
ARG GIT_VERSION=""
RUN VERSION="${GIT_VERSION}"; [ -n "${VERSION}" ] || VERSION="$(git rev-parse --short HEAD 2>/dev/null || echo dev)"; \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/ipv6-proxy-pool ./cmd/ipv6-proxy-pool

FROM alpine:3.22
RUN apk add --no-cache ca-certificates && addgroup -S app && adduser -S -G app app
WORKDIR /app
COPY --from=builder /out/ipv6-proxy-pool /usr/local/bin/ipv6-proxy-pool
COPY web ./web
COPY config.example.json ./config.json
EXPOSE 10080 10070
ENTRYPOINT ["ipv6-proxy-pool"]
CMD ["-config", "/app/config.json"]