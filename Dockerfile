# Base images, the Go module proxy and the Alpine package mirror can be
# overridden (see .env.example) when Docker Hub / proxy.golang.org are slow
# or unreachable, e.g. in mainland China.
ARG GO_IMAGE=golang:1.27-alpine
ARG RUNTIME_IMAGE=alpine:3.22

FROM ${GO_IMAGE} AS build
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY} CGO_ENABLED=0
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -trimpath -ldflags="-s -w" -o /out/ai-route .

FROM ${RUNTIME_IMAGE}
ARG ALPINE_MIRROR=
RUN if [ -n "$ALPINE_MIRROR" ]; then sed -i "s#https\?://dl-cdn.alpinelinux.org#${ALPINE_MIRROR}#g" /etc/apk/repositories; fi \
 && apk add --no-cache ca-certificates && adduser -D -u 10001 app && mkdir -p /data && chown app /data
COPY --from=build /out/ai-route /usr/local/bin/ai-route
USER app
ENV LISTEN=:8080 DATA_DIR=/data
VOLUME /data
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["ai-route"]
