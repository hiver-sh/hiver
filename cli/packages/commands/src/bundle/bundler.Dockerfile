FROM sandbox-tar AS sandbox-tar-src

FROM alpine AS tar-builder
RUN apk add --no-cache jq
COPY --from=sandbox-tar-src sandbox.tar /sandbox.tar
RUN mkdir -p /mnt/rootfs \
    && tar -xf /sandbox.tar -C /mnt/rootfs \
    && mv /mnt/rootfs/manifest.json /mnt/manifest.json \
    && jq -r '.[0].Layers[]' /mnt/manifest.json \
       | while read -r layer; do tar -xf "/mnt/rootfs/$layer" -C /mnt/rootfs && rm -f "/mnt/rootfs/$layer"; done \
    && mv /mnt/rootfs/blobs /mnt/blobs \
    && rm -rf /mnt/rootfs/index.json /mnt/rootfs/oci-layout /mnt/rootfs/repositories \
    && rm -f /sandbox.tar

# Optionally override the source image's entrypoint (hiver bundle --entrypoint).
# sandboxd reads the agent's entrypoint/cmd at run time from the baked image
# config blob (runc.ExtractImageConfig), so rewriting that blob's
# `.config.Entrypoint` here replaces what the sandbox runs. The override arrives
# as a JSON argv array (the CLI tokenizes the string), set as exec-form
# Entrypoint so the command runs directly — no `/bin/sh -c` wrapper, which a
# distroless/scratch image may lack — and Cmd is cleared so it isn't appended as
# stray args. The blob isn't re-hashed — nothing verifies its digest at run
# time, and manifest.json still points at the same path. Falls back to the
# in-rootfs config path for legacy (non-OCI) tars.
ARG ENTRYPOINT_OVERRIDE=
RUN if [ -n "$ENTRYPOINT_OVERRIDE" ]; then \
      cfg=$(jq -r '.[0].Config' /mnt/manifest.json); \
      path="/mnt/$cfg"; [ -f "$path" ] || path="/mnt/rootfs/$cfg"; \
      jq --argjson ep "$ENTRYPOINT_OVERRIDE" \
         '.config.Entrypoint = $ep | .config.Cmd = null' \
         "$path" > "$path.tmp" \
      && mv "$path.tmp" "$path"; \
    fi

# ── extend sandbox-runtime with the pre-bundled agent tar ─────────────────────
FROM hiversh/core AS bundle
# Marks the image as a Hiver bundle so tooling can tell a bundled runtime image
# apart from a raw agent image via `docker image inspect` (no need to crack open
# the rootfs). The microvm stage below inherits it through `FROM bundle`.
LABEL hiver.bundle=1
RUN mkdir -p /mnt
COPY --from=tar-builder /mnt /mnt

# ── microvm-builder: bake sbxguest into the guest rootfs and build rootfs.ext4 ─
# Throwaway stage: it needs the full flattened /mnt/rootfs to produce the ext4,
# but that tree is NOT carried into the final microvm image (see below).
#
# microvm backend: the agent rootfs at /mnt/rootfs becomes the guest root
# drive, so the guest init (sbxguest, matching init=/usr/bin/sbxguest in the
# boot args) must already live inside it. sbxfuse is deliberately NOT baked in:
# it runs host-side for both backends (the guest reaches workspaces over
# 9p-over-vsock), so a copy in the guest rootfs would be dead weight the
# workload could see. The run/sbxguest and mnt/{overlay,merged} dirs are
# mountpoints the guest needs during boot — the read-only root can't mkdir them
# — and sbxguest whiteouts them out of the workload view after pivot.
FROM bundle AS microvm-builder
RUN mkdir -p /mnt/rootfs/usr/bin \
    && cp /usr/local/lib/hive/sbxguest /mnt/rootfs/usr/bin/sbxguest \
    && mkdir -p \
        /mnt/rootfs/run/sbxguest \
        /mnt/rootfs/mnt/overlay \
        /mnt/rootfs/mnt/merged

# Pre-build the guest's read-only root drive (rootfs.ext4) at bundle time. The
# rootfs is identical for every sandbox of this image and is attached read-only,
# so building it once here lets the host share the one baked file directly and
# skip the per-boot `mke2fs -d` over the whole tree — the dominant chunk of
# microvm cold-start (see internal/isolation/microvm.go MountRoot). Must run
# after sbxguest + the boot mountpoints are in /mnt/rootfs so they're baked in.
# Sized at 1.5x the tree as a sparse file, so the generous headroom costs
# nothing on disk.
RUN bytes=$(du -sb /mnt/rootfs | cut -f1) \
    && mib=$(( bytes / 1024 / 1024 + 64 )) \
    && mib=$(( mib + mib / 2 )) \
    && truncate -s "${mib}M" /mnt/rootfs.ext4 \
    && mke2fs -q -F -t ext4 -d /mnt/rootfs /mnt/rootfs.ext4

# ── microvm: final image — runtime base + only what microvm reads at run time ─
# Assembled FROM hiversh/core (not `FROM bundle`) so the flattened /mnt/rootfs
# never lands in this image's layers: the guest's root lives entirely in
# rootfs.ext4, and unlike the container backend microvm never consumes
# /mnt/rootfs at run time (no host-side overlay lower, and workspace config
# arrives via ConfigUpdate / is seeded in the guest — not from a host copy of
# the tree). Starting from a clean base matters because a trailing `rm` could
# not reclaim the tree once it's in an inherited lower layer. We copy only:
#   - manifest.json + the relocated OCI blobs — read by runc.ExtractImageConfig
#     for the agent's entrypoint/env/cwd/exposed ports.
#   - rootfs.ext4 — the prebuilt read-only guest root drive (microvm.MountRoot).
FROM hiversh/core AS microvm
LABEL hiver.bundle=1
RUN mkdir -p /mnt
COPY --from=microvm-builder /mnt/manifest.json /mnt/manifest.json
COPY --from=microvm-builder /mnt/blobs /mnt/blobs
COPY --from=microvm-builder /mnt/rootfs.ext4 /mnt/rootfs.ext4
