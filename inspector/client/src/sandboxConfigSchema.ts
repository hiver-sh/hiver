// Derived from api/config.yaml — SandboxConfig and all referenced schemas.
export const SANDBOX_CONFIG_SCHEMA = {
  $schema: "http://json-schema.org/draft-07/schema#",
  title: "SandboxConfig",
  $ref: "#/definitions/SandboxConfig",
  definitions: {
    SandboxConfig: {
      type: "object",
      description: "Hive sandbox configuration.",
      required: ["fs"],
      additionalProperties: false,
      properties: {
        image: {
          type: "string",
          description: "Reference to the agent image to launch. Cannot be changed after the sandbox is initialized.",
          examples: ["my-agent:latest"],
        },
        env: {
          type: "object",
          additionalProperties: { type: "string" },
          description: "Additional environment variables as a key/value map. Cannot be changed after the sandbox is initialized.",
          examples: [{ LOG_LEVEL: "info", REGION: "us-east-1" }],
        },
        ttl: {
          type: "integer",
          minimum: 0,
          default: 1800,
          description: "Sandbox time to live in seconds. The client must ping /v1/ping to reset the timer. Use 0 to disable shutdown.",
          examples: [1800],
        },
        fs: {
          type: "array",
          minItems: 1,
          description: "One or more file systems exposed to the agent. Mount paths must be unique and non-overlapping.",
          items: { $ref: "#/definitions/FileSystem" },
        },
        egress: {
          type: "array",
          description: "Ordered list of egress rules. First matching rule wins; unmatched requests are denied.",
          items: { $ref: "#/definitions/EgressRule" },
        },
        snapshot: { $ref: "#/definitions/Snapshot" },
      },
    },

    Snapshot: {
      type: "object",
      description: "Snapshot configuration. Captured automatically before the sandbox shuts down and restored before it starts.",
      additionalProperties: false,
      properties: {
        restore_key: {
          type: "string",
          pattern: "^[A-Za-z0-9_-]{1,64}$",
          description: "Key identifying the snapshot to restore when the sandbox starts. When omitted, no snapshot is restored.",
        },
        write_key: {
          type: "string",
          pattern: "^[A-Za-z0-9_-]{1,64}$",
          description: "Key under which the snapshot is saved on shutdown. When omitted, restore_key is used.",
        },
        include: {
          type: "array",
          minItems: 1,
          description: "Glob patterns specifying which paths to include in the snapshot (e.g. /home/user/*).",
          items: { type: "string" },
          examples: [["/home/user/*", "/workspace/data"]],
        },
      },
    },

    FileSystem: {
      description: "A file system exposed to the agent at `mount`. The `backend` selects the storage type. If `acls` is omitted, a default rule granting `rw` access to `<mount>/**` is used.",
      oneOf: [
        { $ref: "#/definitions/LocalFileSystem" },
        { $ref: "#/definitions/GDriveFileSystem" },
        { $ref: "#/definitions/GCSFileSystem" },
      ],
    },

    FileSystemBase: {
      type: "object",
      required: ["mount", "backend"],
      properties: {
        mount: {
          type: "string",
          description: "Absolute path at which the file system appears to the agent.",
          pattern: "^/.+",
          examples: ["/workspace"],
        },
        backend: { $ref: "#/definitions/Backend" },
        acls: {
          type: "array",
          description: "Access control rules for paths under `mount`, evaluated longest-prefix-first. Deny by default when no rule matches. When omitted, a default rule `{ path: \"<mount>/**\", access: \"rw\" }` is applied.",
          items: { $ref: "#/definitions/ACLRule" },
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
              description: "Local path to mount into the sandbox. Only supported with the Docker runtime (local dev).",
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
            gdrive_access_token: { type: "string", description: "OAuth access token." },
            gdrive_refresh_token: { type: "string", description: "OAuth refresh token." },
            gdrive_client_id: { type: "string", description: "OAuth client ID." },
            gdrive_client_secret: { type: "string", description: "OAuth client secret." },
            gdrive_service_account_json: {
              type: "string",
              description: "Service account credential JSON. Mutually exclusive with the OAuth fields.",
            },
            gdrive_folder_id: {
              type: "string",
              description: "ID of the Drive folder the file system is scoped to. Defaults to the account root.",
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
          required: ["gcs_bucket"],
          properties: {
            backend: { type: "string", enum: ["gcs"] },
            gcs_bucket: { type: "string", description: "GCS bucket name." },
            gcs_prefix: {
              type: "string",
              description: "Optional key prefix within the bucket (e.g. workspace/session-42). Defaults to the bucket root.",
            },
            gcs_service_account_json: {
              type: "string",
              description: "Service account credential JSON. Falls back to Application Default Credentials when omitted.",
            },
          },
        },
      ],
    },

    Backend: {
      type: "string",
      enum: ["local", "gdrive", "gcs"],
      description: "Storage type: local (no external deps), gdrive (Google Drive), gcs (Google Cloud Storage).",
    },

    ACLRule: {
      type: "object",
      description: "One access control rule.",
      required: ["path", "access"],
      additionalProperties: false,
      properties: {
        path: {
          type: "string",
          description: "Path or glob the rule applies to (e.g. /workspace/secret/**). Matched longest-prefix-first.",
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
          description: "Exact host (api.github.com) or wildcard suffix (*.pypi.org).",
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
          description: "HTTP methods allowed by this rule. Empty means any method.",
          examples: [["GET"]],
        },
        paths: {
          type: "array",
          items: { type: "string" },
          description: "Glob path patterns allowed by this rule. Empty means any path.",
          examples: [["/repos/*"]],
        },
        override: {
          type: "object",
          additionalProperties: false,
          description: "Values the proxy injects into outbound requests. The agent cannot read these back.",
          properties: {
            query: {
              type: "object",
              additionalProperties: { type: "string" },
              description: "URL query parameters to add or overwrite on the outbound request.",
              examples: [{ apikey: "sk-redacted" }],
            },
            headers: {
              type: "object",
              additionalProperties: { type: "string" },
              description: "HTTP headers to add or overwrite on the outbound request.",
              examples: [{ Authorization: "Bearer sk-redacted", "X-Sandbox-Tenant": "acme" }],
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
