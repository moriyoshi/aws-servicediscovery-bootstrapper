ARG TIKV_VERSION=latest
ARG MUSTER_IMAGE=muster:e2e

FROM ${MUSTER_IMAGE} AS muster

FROM pingcap/pd:${TIKV_VERSION}
COPY --from=muster /muster /muster
COPY pd.star /etc/muster/pd.star

# The real entrypoint is set by the ECS task definition, which is where the
# namespace and kv table names live. This keeps `docker run` on the image
# doing something sensible anyway.
ENTRYPOINT ["/muster", "-script", "/etc/muster/pd.star", "--", "/pd-server"]
