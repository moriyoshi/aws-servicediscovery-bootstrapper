ARG TIKV_VERSION=latest
ARG MUSTER_IMAGE=muster:e2e-gcp

FROM ${MUSTER_IMAGE} AS muster

FROM pingcap/pd:${TIKV_VERSION}
COPY --from=muster /muster /muster
COPY pd.star /etc/muster/pd.star

# The worker pool supplies the rest of the flags from its environment; this
# keeps `docker run` on the image doing something sensible anyway.
ENTRYPOINT ["/muster", "-script", "/etc/muster/pd.star", "--", "/pd-server"]
