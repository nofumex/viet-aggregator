# syntax=docker/dockerfile:1

FROM golang:1.25.13-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./

RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/bot \
    ./cmd/bot


FROM alpine:3.24

COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/bot /usr/local/bin/bot

RUN addgroup -S app && adduser -S -G app app

USER app

EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/bot"]
