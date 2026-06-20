// Derived from api/config.yaml — SandboxConfig and all referenced schemas.
export const SANDBOX_CONFIG_SCHEMA = {
  $schema: "http://json-schema.org/draft-07/schema#",
  title: "SandboxConfig",
  $ref: "#/definitions/SandboxConfig",
  definitions: {
    SandboxConfig: {
      type: "object",
      description: "Hive sandbox configuration.",
      additionalProperties: false,
      properties: {
        image: {
          type: "string",
          description:
            "The Docker image to run. Cannot be changed after the sandbox is initialized.",
          examples: ["my-agent:latest"],
        },
        cpu: {
          type: "number",
          exclusiveMinimum: 0,
          default: 1,
          description:
            "Number of virtual CPUs allocated to the sandbox, as a ceiling (the pod CPU limit). May be fractional (e.g. 0.5); the microvm guest vCPU count is this value rounded up, the container enforces it as a CPU quota. Cannot be changed after the sandbox is initialized.",
          examples: [1],
        },
        memory: {
          type: "integer",
          minimum: 128,
          default: 512,
          description:
            "Memory allocated to the sandbox, in MiB (microvm: guest RAM size; container: enforced as a cgroup memory limit). Cannot be changed after the sandbox is initialized.",
          examples: [512],
        },
        entrypoint: {
          type: "array",
          items: { type: "string" },
          description:
            'Override the entrypoint used when the container is run, as an argv array (each element is a separate argument, not shell-split). When omitted, the image\'s default entrypoint is used. e.g. ["tail", "-f", "/dev/null"] blocks indefinitely with near-zero CPU.',
          examples: [["tail", "-f", "/dev/null"]],
        },
        cwd: {
          type: "string",
          description:
            "Working directory for the entrypoint. When set, the entrypoint is launched with this as its current working directory, overriding the image's default working directory. When omitted, the image's working directory is used. Cannot be changed after the sandbox is initialized.",
          examples: ["/workspace"],
        },
        tty: {
          type: "boolean",
          default: false,
          description:
            "When true, the entrypoint is launched attached to a pseudo-TTY. A client can then attach to that terminal via the exec-stream endpoint with an empty command. Only supported for the container isolation.",
          examples: [false],
        },
        env: {
          type: "object",
          additionalProperties: { type: "string" },
          description:
            "Additional environment variables as a key/value map. Cannot be changed after the sandbox is initialized.",
          examples: [{ LOG_LEVEL: "info", REGION: "us-east-1" }],
        },
        ttl: {
          type: "integer",
          minimum: 0,
          default: 1800,
          description:
            "Sandbox time to live in seconds. The client must ping /v1/ping to reset the timer. Use 0 to disable shutdown.",
          examples: [1800],
        },
        fs: {
          type: "array",
          minItems: 1,
          description:
            "One or more file systems exposed to the agent. Mount paths must be unique and non-overlapping.",
          items: { $ref: "#/definitions/FileSystem" },
        },
        egress: {
          type: "array",
          description:
            "Ordered list of egress rules. First matching rule wins; unmatched requests are denied.",
          items: { $ref: "#/definitions/EgressRule" },
        },
        snapshot: { $ref: "#/definitions/Snapshot" },
      },
    },

    Snapshot: {
      type: "object",
      description:
        "Snapshot configuration. Captured automatically before the sandbox shuts down and restored before it starts.",
      additionalProperties: false,
      properties: {
        restore_key: {
          type: "string",
          pattern: "^[A-Za-z0-9_-]{1,64}$",
          description:
            "Key identifying the snapshot to restore when the sandbox starts. When omitted, no snapshot is restored.",
        },
        write_key: {
          type: "string",
          pattern: "^[A-Za-z0-9_-]{1,64}$",
          description:
            "Key under which the snapshot is saved on shutdown. When omitted, restore_key is used.",
        },
        include: {
          type: "array",
          minItems: 1,
          description:
            "Glob patterns specifying which paths to include in the snapshot (e.g. /home/user/*).",
          items: { type: "string" },
          examples: [["/home/user/*", "/workspace/data"]],
        },
        mount: {
          type: "string",
          pattern: "^/.+",
          description:
            "Mount path of a file system (see fs) where snapshot tarballs are written and read, instead of the host's local snapshot directory. Point it at an internal, remote-backed file system to persist and restore snapshots through a FUSE drive.",
          examples: ["/snapshots"],
        },
      },
    },

    FileSystem: {
      description:
        "A file system exposed to the agent at `mount`. The `backend` selects the storage type. If `acls` is omitted, a default rule granting `rw` access to `<mount>/**` is used.",
      oneOf: [
        { $ref: "#/definitions/LocalFileSystem" },
        { $ref: "#/definitions/GDriveFileSystem" },
        { $ref: "#/definitions/GCSFileSystem" },
        { $ref: "#/definitions/ExternalFileSystem" },
      ],
    },

    FileSystemBase: {
      type: "object",
      required: ["mount", "backend"],
      properties: {
        mount: {
          type: "string",
          description:
            "Absolute path at which the file system appears to the agent.",
          pattern: "^/.+",
          examples: ["/workspace"],
        },
        backend: { $ref: "#/definitions/Backend" },
        acls: {
          type: "array",
          description:
            'Access control rules for paths under `mount`, evaluated longest-prefix-first. Deny by default when no rule matches. When omitted, a default rule `{ path: "<mount>/**", access: "rw" }` is applied.',
          items: { $ref: "#/definitions/ACLRule" },
        },
        internal: {
          type: "boolean",
          default: false,
          description:
            "When true, the file system is mounted inside the sandbox runtime but hidden from the agent workload. Use it for storage the sandbox needs but the agent must not access, e.g. a remote-backed snapshot target referenced by snapshot.mount. Because the agent cannot reach the mount, acls are ignored for internal file systems.",
        },
      },
    },

    LocalFileSystem: {
      description: "Sandbox-local storage with no external dependency.",
      allOf: [
        { $ref: "#/definitions/FileSystemBase" },
        {
          type: "object",
          properties: {
            backend: { type: "string", enum: ["local"] },
            origin: {
              type: "string",
              description:
                "Local path to mount into the sandbox. Only supported with the Docker runtime (local dev).",
            },
          },
        },
      ],
    },

    GDriveFileSystem: {
      description: "A file system backed by Google Drive.",
      allOf: [
        { $ref: "#/definitions/FileSystemBase" },
        {
          type: "object",
          properties: {
            backend: { type: "string", enum: ["gdrive"] },
            gdrive_access_token: {
              type: "string",
              description: "OAuth access token.",
            },
            gdrive_refresh_token: {
              type: "string",
              description: "OAuth refresh token.",
            },
            gdrive_client_id: {
              type: "string",
              description: "OAuth client ID.",
            },
            gdrive_client_secret: {
              type: "string",
              description: "OAuth client secret.",
            },
            gdrive_service_account_json: {
              type: "string",
              description:
                "Service account credential JSON. Mutually exclusive with the OAuth fields.",
            },
            gdrive_folder_id: {
              type: "string",
              description:
                "ID of the Drive folder the file system is scoped to. Defaults to the account root.",
            },
            gdrive_prefix: {
              type: "string",
              description:
                "Optional subfolder path within gdrive_folder_id (e.g. e2e-test/run-42). Created if absent. Defaults to the folder root.",
            },
          },
        },
      ],
    },

    GCSFileSystem: {
      description: "A file system backed by Google Cloud Storage.",
      allOf: [
        { $ref: "#/definitions/FileSystemBase" },
        {
          type: "object",
          required: ["gcs_bucket", "gcs_service_account_json"],
          properties: {
            backend: { type: "string", enum: ["gcs"] },
            gcs_bucket: { type: "string", description: "GCS bucket name." },
            gcs_prefix: {
              type: "string",
              description:
                "Optional key prefix within the bucket (e.g. workspace/session-42). Defaults to the bucket root.",
            },
            gcs_service_account_json: {
              type: "string",
              description:
                "Service account credential JSON. Falls back to Application Default Credentials when omitted.",
            },
          },
        },
      ],
    },

    ExternalFileSystem: {
      description:
        "A file system backed by an external HTTP host implementing the external file system interface.",
      allOf: [
        { $ref: "#/definitions/FileSystemBase" },
        {
          type: "object",
          required: ["host"],
          properties: {
            backend: { type: "string", enum: ["external"] },
            host: {
              type: "string",
              description:
                "Base URL of the host implementing the external file system HTTP interface. Store operations are issued relative to this URL (e.g. GET <host>/v1/file?path=...). A trailing slash is ignored.",
              examples: ["https://fs.internal:8080"],
            },
          },
        },
      ],
    },

    Backend: {
      type: "string",
      enum: ["local", "gdrive", "gcs", "external"],
      description:
        "Storage type: local (no external deps), gdrive (Google Drive), gcs (Google Cloud Storage), external (HTTP host).",
    },

    ACLRule: {
      type: "object",
      description: "One access control rule.",
      required: ["path", "access"],
      additionalProperties: false,
      properties: {
        path: {
          type: "string",
          description:
            "Path or glob the rule applies to (e.g. /workspace/secret/**). Matched longest-prefix-first.",
          examples: ["/workspace/secret/**"],
        },
        access: {
          type: "string",
          enum: ["rw", "ro", "deny"],
          description: "Read-write, read-only, or fully denied.",
        },
      },
    },

    EgressRule: {
      type: "object",
      description: "One egress rule.",
      required: ["host", "access"],
      additionalProperties: false,
      properties: {
        access: {
          type: "string",
          enum: ["allow", "deny"],
          description: "Whether matching requests are allowed or denied.",
        },
        host: {
          type: "string",
          description:
            "Exact host (api.github.com) or wildcard suffix (*.pypi.org).",
          examples: ["api.github.com"],
        },
        ports: {
          type: "array",
          items: { type: "integer" },
          description: "Optional port constraints. Empty means any port.",
          examples: [[443]],
        },
        methods: {
          type: "array",
          items: { $ref: "#/definitions/HttpMethod" },
          description:
            "HTTP methods allowed by this rule. Empty means any method.",
          examples: [["GET"]],
        },
        paths: {
          type: "array",
          items: { type: "string" },
          description:
            "Glob path patterns allowed by this rule. Empty means any path.",
          examples: [["/repos/*"]],
        },
        override: {
          type: "object",
          additionalProperties: false,
          description:
            "Values the proxy injects into outbound requests. The agent cannot read these back.",
          properties: {
            host: {
              type: "string",
              description:
                "Upstream the proxy dials instead of the matched host, as `hostname[:port]`. When the port is omitted, the original destination port is kept. Matching and the agent-visible request (Host header, SNI) keep the original hostname.",
              examples: ["stub.internal:17080"],
            },
            prefix_path: {
              type: "string",
              description:
                "Path prefix prepended to the outbound request path (`/mock` turns `/v1/user` into `/mock/v1/user`). Matching and audit events keep the original path. Inspected HTTP only.",
              examples: ["/mock"],
            },
            query: {
              type: "object",
              additionalProperties: { type: "string" },
              description:
                "URL query parameters to add or overwrite on the outbound request.",
              examples: [{ apikey: "sk-redacted" }],
            },
            headers: {
              type: "object",
              additionalProperties: { type: "string" },
              description:
                "HTTP headers to add or overwrite on the outbound request.",
              examples: [
                {
                  Authorization: "Bearer sk-redacted",
                  "X-Sandbox-Tenant": "acme",
                },
              ],
            },
          },
        },
      },
    },

    HttpMethod: {
      type: "string",
      enum: ["GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"],
    },
  },
};
