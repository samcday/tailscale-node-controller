FROM --platform=$BUILDPLATFORM alpine:3.13
RUN apk add --no-cache ca-certificates
ENTRYPOINT ["/tailscale-node-controller"]
COPY tailscale-node-controller /
