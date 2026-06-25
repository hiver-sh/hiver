// Persist a sandbox's filesystem across a full shutdown by writing the
// snapshot tarball through a FUSE drive instead of the sandbox host's local
// disk.
//
// Two new pieces make this work:
//   - An `internal` file system: it is mounted inside the sandbox runtime but
//     hidden from the agent — the agent never sees `/snapshot-drive`. Here it is
//     backed by Google Cloud Storage, so its contents live in a GCS bucket.
//   - `snapshot.mount`: points the snapshot machinery at that mount, so the
//     tarball is captured to (and restored from) `/snapshot-drive` — i.e. GCS —
//     rather than the host's local snapshot directory.
//
// The upshot: the snapshot survives even if the next boot lands on a different
// host, because it is stored in the bucket, not on local disk.
//
// Requires GCS_BUCKET and GCS_SERVICE_ACCOUNT_JSON in the environment.
//
// Run with: npx tsx examples/snapshot-fuse.ts
import process from "node:process";

import * as hiver from "@hiver.sh/client";

const bucket = process.env.GCS_BUCKET;
const serviceAccountJson = process.env.GCS_SERVICE_ACCOUNT_JSON;
if (!bucket || !serviceAccountJson) {
  console.error(
    "set GCS_BUCKET and GCS_SERVICE_ACCOUNT_JSON to a bucket and service account credential JSON",
  );
  process.exit(1);
}

const ID = "hiver-snapshot-fuse-example";
// Unique per run so every invocation starts fresh (no prior snapshot to restore)
// and writes its own tarball to GCS, rather than reusing a fixed key. Must match
// the snapshot key shape: [A-Za-z0-9_-]{1,64}.
const KEY = `snapshot-fuse-example-${Date.now()}`;

// Config shared by both boots:
//   - /snapshot-drive is an internal GCS mount used purely as the snapshot drive.
//   - snapshot.mount routes the tarball through /snapshot-drive (→ GCS).
//   - /root is captured on shutdown and restored on start.
const config: hiver.SandboxConfig = {
  image: "node",
  fs: [
    {
      backend: "gcs",
      mount: "/snapshot-drive",
      internal: true,
      gcs_bucket: bucket,
      gcs_prefix: "hiver-snapshots",
      gcs_service_account_json: serviceAccountJson,
    },
  ],
  snapshot: {
    restore_key: KEY,
    write_key: KEY,
    mount: "/snapshot-drive",
    include: ["/root/**"],
  },
};

// shut down (captures the snapshot to GCS). ---
const first = await hiver.getOrCreateSandbox(ID, config);

const before = await first.exec(
  "echo 'hello from the first boot' > /root/note.txt && cat /root/note.txt",
);
console.info("wrote /root/note.txt:", before.stdout.trim());

const hidden = await first.exec("test -e /snapshot-drive; echo $?");
console.info(
  "agent sees /snapshot-drive?",
  hidden.stdout.trim() === "0" ? "yes (unexpected)" : "no (as expected)",
);

console.info("shutting down (capturing snapshot to GCS)…");
await first.shutdown();

// GCS through the FUSE drive.
const second = await hiver.getOrCreateSandbox(ID, config);

const after = await second.exec("cat /root/note.txt");
console.info("restored /root/note.txt:", after.stdout.trim());

if (after.exit_code !== 0) {
  console.error("expected the file to be restored from the snapshot");
  process.exitCode = 1;
}

await second.shutdown();
