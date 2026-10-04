FROM gcr.io/distroless/static:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3

ENTRYPOINT ["/usr/bin/aludel-cloudscale"]
COPY aludel-cloudscale /usr/bin/
