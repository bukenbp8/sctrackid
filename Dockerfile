# syntax=docker/dockerfile:1

FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
COPY web ./web
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/sctrackid .

FROM alpine:3.22
# ffmpeg cuts the audio samples for Shazam
RUN apk add --no-cache ffmpeg ca-certificates tzdata \
 && adduser -D -H -u 10001 sctrackid \
 && mkdir /data && chown sctrackid /data
COPY --from=build /out/sctrackid /usr/local/bin/sctrackid
USER sctrackid
WORKDIR /data
# the library database, Shazam/link caches and saved .txt tracklists all live in /data
ENV XDG_CACHE_HOME=/data/cache \
    SCTRACKID_DB=/data/sctrackid.db
VOLUME /data
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s \
  CMD wget -qO /dev/null http://127.0.0.1:8080/robots.txt || exit 1
ENTRYPOINT ["sctrackid"]
CMD ["-serve", ":8080"]
