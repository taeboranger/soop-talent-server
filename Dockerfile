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

FROM gcr.io/distroless/static-debian12:nonroot AS app
WORKDIR /app
COPY --from=builder --chown=nonroot:nonroot /out/sooptalent ./sooptalent
COPY --from=builder --chown=nonroot:nonroot /out/sooptalent-migrate ./sooptalent-migrate
COPY --from=builder --chown=nonroot:nonroot /src/migrations ./migrations
ENV LISTEN=:8080 \
    MIGRATIONS=./migrations \
    LOG_LEVEL=info
EXPOSE 8080
ENTRYPOINT ["/app/sooptalent"]
