FROM --platform=$BUILDPLATFORM node:22-bookworm-slim AS frontend
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM --platform=$BUILDPLATFORM golang:1.25-bookworm AS backend
ARG TARGETOS=linux
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
COPY --from=frontend /src/internal/webui/dist/ internal/webui/dist/
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/spoticonn ./cmd/spoticonn

FROM --platform=$BUILDPLATFORM debian:bookworm-slim AS engines
ARG TARGETARCH
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl && rm -rf /var/lib/apt/lists/*
COPY scripts/fetch-engines.sh /fetch-engines.sh
RUN sh /fetch-engines.sh "$TARGETARCH" /engines

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates libasound2 libstdc++6 libgcc-s1 \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd -g 10001 spoticonn && useradd -u 10001 -g 10001 -M -s /usr/sbin/nologin spoticonn \
    && mkdir -p /data /run/spoticonn && chown -R 10001:10001 /data /run/spoticonn
COPY --from=backend /out/spoticonn /usr/local/bin/spoticonn
COPY --from=engines /engines/ /usr/local/bin/
COPY third_party/ /usr/share/licenses/spoticonn/
USER 10001:10001
ENV SPOTICONN_DATA_DIR=/data SPOTICONN_RUNTIME_DIR=/run/spoticonn SPOTICONN_LISTEN_ADDR=127.0.0.1:8080
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s CMD ["spoticonn", "healthcheck"]
ENTRYPOINT ["spoticonn"]
