FROM golang:1.23 AS go-build

WORKDIR /src
COPY go.mod ./
COPY cmd/ cmd/
COPY internal/ internal/
RUN go build -o /out/xiaoli-admin ./cmd/xiaoli-admin

FROM debian:bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*

COPY --from=go-build /out/xiaoli-admin /usr/local/bin/xiaoli-admin

ENV XIAOLI_ADMIN_HOST=0.0.0.0
ENV XIAOLI_ADMIN_PORT=8080
ENV XIAOLI_DIRECT_DEVICE_SERVER=true

EXPOSE 8080

CMD ["/usr/local/bin/xiaoli-admin"]
