FROM golang:1.27.3-trixie AS build
RUN apt-get update \
    && apt-get install --yes --no-install-recommends libheif-dev pkg-config \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY openapi.go openapi.json ./
COPY cmd/ ./cmd/
COPY internal/ ./internal/
RUN CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -o /out/tt-scrap ./cmd/tt-scrap

FROM debian:trixie-slim AS runtime
RUN apt-get update \
    && apt-get install --yes --no-install-recommends ca-certificates libheif1 libheif-plugin-libde265 libheif-plugin-dav1d \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --uid 10001 --create-home --shell /usr/sbin/nologin scrap
WORKDIR /app
COPY --from=build /out/tt-scrap /usr/local/bin/tt-scrap
USER scrap
EXPOSE 8000
HEALTHCHECK --interval=30s --timeout=3s --start-period=5m --retries=3 \
    CMD ["tt-scrap", "healthcheck"]
ENTRYPOINT ["tt-scrap"]
