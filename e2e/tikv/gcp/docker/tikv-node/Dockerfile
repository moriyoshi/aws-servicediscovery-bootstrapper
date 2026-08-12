ARG TIKV_VERSION=latest
ARG MUSTER_IMAGE=muster:e2e-gcp

FROM ${MUSTER_IMAGE} AS muster

FROM pingcap/tikv:${TIKV_VERSION}
COPY --from=muster /muster /muster
COPY tikv.star /etc/muster/tikv.star

ENTRYPOINT ["/muster", "-script", "/etc/muster/tikv.star", "--", "/tikv-server"]
