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

FROM debian:bookworm-slim AS onnxrt

ARG ONNXRUNTIME_VERSION=1.20.1
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl tar \
    && rm -rf /var/lib/apt/lists/*
RUN curl -fsSL -o /tmp/ort.tgz \
        "https://github.com/microsoft/onnxruntime/releases/download/v${ONNXRUNTIME_VERSION}/onnxruntime-linux-x64-${ONNXRUNTIME_VERSION}.tgz" \
    && mkdir -p /opt/onnxruntime \
    && tar -xzf /tmp/ort.tgz -C /opt/onnxruntime --strip-components=1 \
    && rm /tmp/ort.tgz

FROM debian:bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates libopus0 libopusfile0 libstdc++6 \
    && rm -rf /var/lib/apt/lists/*

COPY --from=onnxrt /opt/onnxruntime/lib/libonnxruntime.so* /usr/local/lib/
RUN ldconfig

COPY --from=go-build /out/xiaoli-admin /usr/local/bin/xiaoli-admin

ENV XIAOLI_ADMIN_HOST=0.0.0.0
ENV XIAOLI_ADMIN_PORT=8080
ENV XIAOLI_DIRECT_DEVICE_SERVER=true
ENV XIAOLI_ONNXRUNTIME_PATH=/usr/local/lib/libonnxruntime.so

EXPOSE 8080

CMD ["/usr/local/bin/xiaoli-admin"]
