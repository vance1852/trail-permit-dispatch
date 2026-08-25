# syntax=docker/dockerfile:1

# The build stage always runs natively and cross compiles for the target
# architecture, which keeps linux/amd64 and linux/arm64 images reproducible.
FROM --platform=$BUILDPLATFORM golang:1.22.5-alpine3.20 AS build

ENV CGO_ENABLED=0 \
    GOTOOLCHAIN=local

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal
COPY migrations ./migrations

ARG TARGETOS
ARG TARGETARCH
RUN GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -ldflags="-s -w" -o /out/trail-permit-server ./cmd/server

FROM alpine:3.20

RUN adduser -D -u 10001 trail \
    && mkdir -p /app/data \
    && chown -R trail:trail /app

WORKDIR /app
COPY --from=build /out/trail-permit-server /usr/local/bin/trail-permit-server

USER trail

ENV HTTP_ADDR=:8080 \
    DATABASE_DSN=file:/app/data/trail-permit.sqlite \
    LOG_LEVEL=info \
    SEED_ENABLED=true

EXPOSE 8080
VOLUME ["/app/data"]

HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 \
    CMD wget -q -O - http://127.0.0.1:8080/healthz > /dev/null 2>&1 || exit 1

ENTRYPOINT ["/usr/local/bin/trail-permit-server"]
