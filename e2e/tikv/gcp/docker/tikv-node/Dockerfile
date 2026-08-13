ARG TIKV_VERSION=latest
ARG MUSTER_IMAGE=muster:e2e-gcp

FROM ${MUSTER_IMAGE} AS muster

FROM pingcap/tikv:${TIKV_VERSION}
COPY --from=muster /muster /muster
COPY tikv.star /etc/muster/tikv.star

# --config, not flags: max-open-files has no command-line equivalent, and it has
# to come down for TiKV to start at all inside Cloud Run's file-descriptor cap.
# See tikv.toml. The script's flags are appended after this and win where they
# overlap, which they do not.
COPY tikv.toml /etc/tikv/config.toml

ENTRYPOINT ["/muster", "-script", "/etc/muster/tikv.star", "--", "/tikv-server", "--config", "/etc/tikv/config.toml"]
