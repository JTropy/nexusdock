FROM node:26-alpine AS web-builder
WORKDIR /src/web
COPY web/package*.json ./
RUN npm ci
COPY web ./
COPY internal/httpx/web_dist ../internal/httpx/web_dist
RUN npm run build

FROM golang:1.26-alpine AS go-builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web-builder /src/internal/httpx/web_dist ./internal/httpx/web_dist
RUN go build -o /out/nexusdock ./cmd/nexusdock

FROM alpine:3.20
RUN apk add --no-cache git ca-certificates \
    && addgroup -S -g 10001 nexus \
    && adduser -S -D -H -u 10001 -G nexus nexus \
    && mkdir -p /var/lib/nexus /recall \
    && chown -R 10001:10001 /var/lib/nexus /recall
WORKDIR /app
COPY --from=go-builder /out/nexusdock /usr/local/bin/nexusdock
ENV NEXUS_HOST=0.0.0.0 \
    NEXUS_PORT=18777 \
    NEXUS_DATA_DIR=/var/lib/nexus \
    RECALL_REPO_DIR=/recall \
    HOME=/tmp
EXPOSE 18777
VOLUME ["/var/lib/nexus", "/recall"]
USER 10001:10001
# HEALTHCHECK 打 /ready（readiness）：控制库可查询且 Recall 根目录可访问才算健康；
# /health 保持为极轻量 liveness，不做依赖检查，不能反映数据面是否可用。
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 CMD wget -q -T 2 -O /dev/null http://127.0.0.1:18777/ready || exit 1
ENTRYPOINT ["nexusdock"]
