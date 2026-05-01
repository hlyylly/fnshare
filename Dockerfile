# syntax=docker/dockerfile:1.7

# ---- build stage ----
FROM golang:1.22-alpine AS build
WORKDIR /src
RUN apk add --no-cache git ca-certificates

COPY go.mod ./
COPY . .
RUN go mod tidy
RUN CGO_ENABLED=0 GOFLAGS="-trimpath" \
    go build -ldflags="-s -w" -o /out/fnshare ./cmd/fnshare

# ---- runtime stage ----
FROM alpine:3.20
# fuse3: enables the optional FUSE mount (`fnshare daemon` with --mount or
# FNSHARE_MOUNT env). Container needs cap_add SYS_ADMIN and /dev/fuse.
RUN apk add --no-cache ca-certificates tini fuse3 \
 && echo "user_allow_other" >> /etc/fuse.conf
COPY --from=build /out/fnshare /usr/local/bin/fnshare

ENV FNSHARE_DATA=/data
VOLUME ["/data"]

# libp2p TCP + QUIC (UDP), v4 + v6 share the same port
EXPOSE 4001/tcp 4001/udp 4101/tcp

ENTRYPOINT ["/sbin/tini", "--", "/usr/local/bin/fnshare"]
CMD ["daemon"]
