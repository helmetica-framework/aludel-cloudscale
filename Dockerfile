FROM gcr.io/distroless/static:nonroot@sha256:516757106de35205efb70b91ba03f6f1298424a0678c603255cf2852a7000087

ENTRYPOINT ["/usr/bin/aludel-cloudscale"]
COPY aludel-cloudscale /usr/bin/
