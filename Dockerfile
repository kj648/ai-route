# syntax=docker/dockerfile:1
FROM golang:1.27-alpine AS build
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY} CGO_ENABLED=0
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -trimpath -ldflags="-s -w" -o /out/ai-route .

FROM alpine:3.22
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 app && mkdir -p /data && chown app /data
COPY --from=build /out/ai-route /usr/local/bin/ai-route
USER app
ENV LISTEN=:8080 DATA_DIR=/data
VOLUME /data
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["ai-route"]
