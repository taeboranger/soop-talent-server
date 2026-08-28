# syntax=docker/dockerfile:1

# 앱 이미지 안에 서버와 마이그레이션 실행 파일을 함께 넣는다.
# Kubernetes initContainer가 서버 컨테이너보다 먼저 마이그레이션을 실행한다.
FROM golang:1.26-alpine AS builder
WORKDIR /src

RUN apk add --no-cache ca-certificates
COPY go.mod go.sum ./
RUN go mod download
COPY . .

RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/sooptalent ./cmd/sooptalent \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/sooptalent-migrate ./cmd/sooptalent-migrate

FROM node:22-bookworm-slim AS bridge-deps
WORKDIR /bridge
COPY chat-bridge/package.json chat-bridge/package-lock.json ./
RUN npm ci --omit=dev --ignore-scripts

# 공식 Chat SDK가 브라우저 전용이므로 같은 이미지에서 Chromium 사이드카도 실행한다.
# Client Secret과 OAuth 토큰은 이 서버 이미지 밖으로 전달되지 않는다.
FROM node:22-bookworm-slim AS app
RUN apt-get update \
 && apt-get install -y --no-install-recommends chromium ca-certificates \
 && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=builder --chown=node:node /out/sooptalent ./sooptalent
COPY --from=builder --chown=node:node /out/sooptalent-migrate ./sooptalent-migrate
COPY --from=builder --chown=node:node /src/migrations ./migrations
COPY --from=bridge-deps --chown=node:node /bridge/node_modules ./chat-bridge/node_modules
COPY --chown=node:node chat-bridge/package.json chat-bridge/bridge.mjs ./chat-bridge/
ENV LISTEN=:8080 \
    MIGRATIONS=./migrations \
    LOG_LEVEL=info \
    CHROMIUM_PATH=/usr/bin/chromium \
    SERVER_INTERNAL_URL=http://127.0.0.1:8080
# Kubernetes가 runAsNonRoot 정책을 이미지 단계에서 검증할 수 있도록
# 이름(node) 대신 Node 공식 이미지의 숫자 UID:GID를 명시한다.
USER 1000:1000
EXPOSE 8080
ENTRYPOINT ["/app/sooptalent"]
