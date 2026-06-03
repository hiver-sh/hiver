FROM sandbox-tar AS sandbox-tar-src

FROM alpine AS tar-builder
RUN apk add --no-cache jq
COPY --from=sandbox-tar-src sandbox.tar /sandbox.tar
RUN mkdir -p /mnt/rootfs \
    && tar -xf /sandbox.tar -C /mnt/rootfs \
    && mv /mnt/rootfs/manifest.json /mnt/manifest.json \
    && jq -r '.[0].Layers[]' /mnt/manifest.json \
       | while read -r layer; do tar -xf "/mnt/rootfs/$layer" -C /mnt/rootfs && rm -f "/mnt/rootfs/$layer"; done \
    && rm -f /sandbox.tar

# ── extend sandbox-runtime with the pre-bundled agent tar ─────────────────────
FROM hive-sandbox-runtime
RUN mkdir -p /mnt
COPY --from=tar-builder /mnt /mnt
