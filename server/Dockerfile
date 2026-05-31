FROM golang:1.23-bookworm AS go-build

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        gcc \
        libc6-dev \
        libopus-dev \
        libopusfile-dev \
        pkg-config \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
ENV CGO_ENABLED=1
RUN go build -o /out/xiaoli-admin ./cmd/xiaoli-admin

FROM debian:bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates libopus0 libopusfile0 \
    && rm -rf /var/lib/apt/lists/*

COPY --from=go-build /out/xiaoli-admin /usr/local/bin/xiaoli-admin

ENV XIAOLI_ADMIN_HOST=0.0.0.0
ENV XIAOLI_ADMIN_PORT=8080
ENV XIAOLI_DIRECT_DEVICE_SERVER=true

EXPOSE 8080

CMD ["/usr/local/bin/xiaoli-admin"]
