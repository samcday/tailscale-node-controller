#syntax=docker/dockerfile:1.2
FROM golang:1.19 as builder
WORKDIR /usr/src/app
ADD . .
RUN --mount=type=cache,id=gomod,target=/go/pkg/mod \
    --mount=type=cache,id=gobuild,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -o tailscale-node-controller .

FROM alpine:3.13
RUN apk add --no-cache ca-certificates
COPY --from=builder /usr/src/app/tailscale-node-controller /bin/
ENTRYPOINT /bin/tailscale-node-controller
