from __future__ import annotations

from typing import Annotated, Any, Literal, Optional, Union
from pydantic import BaseModel, ConfigDict, Field, TypeAdapter, field_validator


class ACLRule(BaseModel):
    """One access control rule."""

    path: str
    """Path or glob the rule applies to (e.g. ``/workspace/secret/**``). Rules are matched longest-prefix-first; access is denied by default when no rule matches."""
    access: Literal["rw", "ro", "deny"]
    """Read-write, read-only, or fully denied."""


class _FileSystemBase(BaseModel):
    mount: str
    """Absolute path at which the file system appears to the agent."""
    acls: Optional[list[ACLRule]] = None
    """Access control rules for paths under ``mount``."""
    internal: Optional[bool] = None
    """When true, the file system is mounted inside the sandbox runtime but hidden from the agent workload. Use it for storage the sandbox needs but the agent must not see, e.g. a remote-backed snapshot target referenced by ``Snapshot.mount``. Because the agent cannot reach the mount, ``acls`` are ignored for internal file systems."""

    @field_validator("mount")
    @classmethod
    def _mount_must_be_absolute(cls, v: str) -> str:
        if not v.startswith("/"):
            raise ValueError("mount must be an absolute path")
        return v


class LocalFileSystem(_FileSystemBase):
    """Sandbox-local storage with no external dependency."""

    backend: Literal["local"]
    origin: Optional[str] = None
    """The local path to mount into this sandbox. Local origins are only supported locally with the Docker runtime — helpful for local development, e.g. mounting local skill files into the sandbox."""


class GDriveFileSystem(_FileSystemBase):
    """A file system backed by Google Drive."""

    backend: Literal["gdrive"]
    gdrive_access_token: Optional[str] = None
    """OAuth access token."""
    gdrive_refresh_token: Optional[str] = None
    """OAuth refresh token."""
    gdrive_client_id: Optional[str] = None
    """OAuth client ID."""
    gdrive_client_secret: Optional[str] = None
    """OAuth client secret."""
    gdrive_service_account_json: Optional[str] = None
    """Service account credential JSON. Mutually exclusive with the OAuth fields above."""
    gdrive_folder_id: Optional[str] = None
    """ID of the Drive folder the file system is scoped to. When omitted, the account root is used."""
    gdrive_prefix: Optional[str] = None
    """Optional subfolder path within ``gdrive_folder_id`` (e.g. ``e2e-test/run-42``). Created if absent."""


class GCSFileSystem(_FileSystemBase):
    """A file system backed by Google Cloud Storage."""

    backend: Literal["gcs"]
    gcs_bucket: str
    """GCS bucket name."""
    gcs_prefix: Optional[str] = None
    """Optional key prefix within the bucket (e.g. ``workspace/session-42``). When omitted, the bucket root is used."""
    gcs_service_account_json: str
    """Service account credential JSON. When omitted, Application Default Credentials are used (the ``GOOGLE_APPLICATION_CREDENTIALS`` env var, gcloud user credentials, or the GCE/GKE metadata server)."""


class S3FileSystem(_FileSystemBase):
    """A file system backed by Amazon S3 or an S3-compatible service."""

    backend: Literal["s3"]
    s3_bucket: str
    """S3 bucket name."""
    s3_region: Optional[str] = None
    """AWS region of the bucket (e.g. ``us-east-1``). Required for AWS; some S3-compatible services accept ``auto``."""
    s3_prefix: Optional[str] = None
    """Optional key prefix within the bucket (e.g. ``workspace/session-42``). When omitted, the bucket root is used."""
    s3_access_key_id: str
    """Access key ID for the S3 credentials."""
    s3_secret_access_key: str
    """Secret access key for the S3 credentials."""
    s3_session_token: Optional[str] = None
    """Optional session token, for temporary (STS) credentials."""
    s3_endpoint: Optional[str] = None
    """Optional custom endpoint URL for S3-compatible services such as MinIO, Cloudflare R2, or Backblaze B2. When omitted, the standard AWS endpoint for ``s3_region`` is used."""
    s3_use_path_style: Optional[bool] = None
    """Use path-style addressing instead of virtual-hosted. Most S3-compatible services (MinIO, localstack) require this."""


class AzureBlobFileSystem(_FileSystemBase):
    """A file system backed by Azure Blob Storage."""

    backend: Literal["azure"]
    azure_container: str
    """Blob container name (the Azure equivalent of a bucket)."""
    azure_account: Optional[str] = None
    """Storage account name. Required unless ``azure_connection_string`` or ``azure_endpoint`` is set."""
    azure_prefix: Optional[str] = None
    """Optional key prefix within the container (e.g. ``workspace/session-42``). When omitted, the container root is used."""
    azure_account_key: Optional[str] = None
    """Storage account access key (shared-key auth). One of ``azure_account_key``, ``azure_connection_string``, or ``azure_sas_token`` is required."""
    azure_connection_string: Optional[str] = None
    """Full connection string (account, key, and endpoint). Takes precedence over the other credential fields."""
    azure_sas_token: Optional[str] = None
    """Shared access signature token authorizing the container. A leading ``?`` is optional."""
    azure_endpoint: Optional[str] = None
    """Optional custom blob service endpoint (e.g. the Azurite emulator). When omitted, ``https://{azure_account}.blob.core.windows.net`` is used."""


class OneDriveFileSystem(_FileSystemBase):
    """A file system backed by Microsoft OneDrive (via the Microsoft Graph API)."""

    backend: Literal["onedrive"]
    onedrive_access_token: str
    """OAuth access token."""
    onedrive_refresh_token: Optional[str] = None
    """OAuth refresh token; pair with ``onedrive_client_id`` and ``onedrive_client_secret`` to enable token refresh."""
    onedrive_client_id: Optional[str] = None
    """OAuth application (client) ID."""
    onedrive_client_secret: Optional[str] = None
    """OAuth client secret."""
    onedrive_tenant: Optional[str] = None
    """Microsoft identity platform tenant used for token refresh. Defaults to ``common``."""
    onedrive_drive_id: Optional[str] = None
    """Target a specific drive (e.g. a SharePoint document library). When omitted, the signed-in user's OneDrive is used."""
    onedrive_prefix: Optional[str] = None
    """Optional subfolder path the file system is scoped to (e.g. ``e2e-test/run-42``). Created if absent."""


class ExternalFileSystem(_FileSystemBase):
    """A file system backed by an external HTTP host. Each agent file operation becomes one call against ``host``."""

    backend: Literal["external"]
    host: str
    """Base URL of the host implementing the external file system interface. A trailing slash is ignored."""


FileSystem = Annotated[
    Union[LocalFileSystem, GDriveFileSystem, GCSFileSystem, S3FileSystem, AzureBlobFileSystem, OneDriveFileSystem, ExternalFileSystem],
    Field(discriminator="backend"),
]
"""A file system exposed to the agent at ``mount``. ``backend`` selects the storage type. Access is governed by ``acls``, evaluated longest-prefix-first with deny as the default."""

HttpMethod = Literal["GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"]
"""An HTTP method an egress rule can match."""


class EgressOverride(BaseModel):
    """Values the proxy injects into outbound requests that match an egress rule. If the agent already set the same query parameter or header, the proxy overwrites it; otherwise the value is added. The agent cannot read these values back."""

    host: Optional[str] = None
    """Upstream the proxy dials instead of the matched host, as ``hostname[:port]`` or ``ip[:port]``. When the port is omitted, the original destination port is kept. The agent-visible request (Host header, TLS SNI) keeps the original hostname."""
    prefix_path: Optional[str] = None
    """Path prefix prepended to the outbound request path (``/mock`` turns ``/v1/user`` into ``/mock/v1/user``). The agent's original path is preserved for rule matching and audit events. A trailing slash is ignored."""
    query: Optional[dict[str, str]] = None
    """URL query parameters to add or overwrite on the outbound request. Useful for injecting API keys the agent should never see."""
    headers: Optional[dict[str, str]] = None
    """HTTP headers to add or overwrite on the outbound request. Useful for injecting bearer tokens or tenant identifiers."""
    body: Optional[Union[str, dict[str, Any]]] = None
    """Request body the proxy sends upstream in place of the agent's. A string always replaces the body verbatim. An object is applied per ``body_strategy`` (default ``merge``): ``merge`` shallow-merges it into the agent's JSON body (top-level keys here overwrite the agent's, all other keys are preserved — agent ``{"a":1,"b":2}`` with ``{"b":3}`` sends ``{"a":1,"b":3}``; if the agent's body is absent or not a JSON object the override object is sent as-is), while ``replace`` discards the agent's body and sends the override object as-is."""
    body_strategy: Optional[Literal["merge", "replace"]] = None
    """How an object ``body`` override is applied. ``merge`` (the default) shallow-merges the override into the agent's JSON body; ``replace`` discards the agent's body and sends the override object as-is. Ignored when ``body`` is a string."""


class EgressRule(BaseModel):
    """One egress rule."""

    access: Literal["allow", "deny"]
    """Whether matching requests are allowed or denied."""
    host: str
    """Exact host (``api.github.com``) or wildcard suffix (``*.pypi.org``)."""
    ports: Optional[list[int]] = None
    """Optional ports; when omitted no port enforcement is performed."""
    methods: Optional[list[HttpMethod]] = None
    """HTTP methods matched by this rule. Empty means any method."""
    paths: Optional[list[str]] = None
    """Git-style glob path patterns matched by this rule. Empty means any path. Matching is segment-by-segment on ``/`` boundaries. ``*`` matches zero or more characters within a single segment and never crosses ``/`` (``/users/*`` matches ``/users/42`` and ``/users/`` but not ``/users/42/posts``; ``/v*/users`` matches ``/v2/users``). ``**`` matches zero or more whole segments (``/repos/**`` matches ``/repos``, ``/repos/foo``, and ``/repos/foo/bar``; ``/a/**/z`` matches ``/a/z`` and ``/a/b/c/z``)."""
    override: Optional[EgressOverride] = None
    """Values the proxy injects into matching outbound requests."""
    override_script: Optional[str] = None
    """Optional Lua script run against matching inspected HTTP requests, after ``override`` is applied. It can rewrite the request body and headers programmatically. Runs in a restricted VM (base/string/table/math only) with globals ``body`` (string), ``headers`` (name→value table), and read-only ``method``/``host``/``path``/``query``; helpers ``urldecode``/``urlencode``/``b64decode``/``b64encode``. Inspected HTTP only."""


class SnapshotVM(BaseModel):
    """microVM-state snapshot. When a VM snapshot exists under ``key``, a get-or-create resumes it instead of cold-booting; otherwise the VM cold-boots and the client captures the snapshot explicitly. Ignored by the container backend."""

    key: str = Field(..., pattern=r'^[A-Za-z0-9_-]{1,64}$')
    """Key identifying the VM-state snapshot."""


class SnapshotFiles(BaseModel):
    """Writable-filesystem snapshot, captured as a portable gzip-tar. Restored when the sandbox starts and written by the snapshot action (or on shutdown when ``write_on_shutdown`` is set)."""

    key: str = Field(..., pattern=r'^[A-Za-z0-9_-]{1,64}$')
    """Key identifying the files snapshot."""
    write_on_shutdown: Optional[bool] = None
    """When true, the files snapshot is captured on shutdown or termination. When false (the default), files are captured only by an explicit snapshot action."""
    include: Optional[list[str]] = Field(None, min_length=1)
    """Glob patterns specifying which paths to include in the snapshot (e.g. ``/home/user/*``)."""
    mount: Optional[str] = Field(None, pattern=r'^/.+')
    """Mount path of a file system (see ``SandboxConfig.fs``) where the files tarball is written and read, instead of the host's local snapshot directory. Point it at an ``internal``, remote-backed file system to persist and restore through a FUSE drive."""


class Snapshot(BaseModel):
    """Snapshot configuration. It has two independent parts: ``vm`` captures the full microVM state (a no-op for the container backend) and ``files`` captures the writable filesystem as a portable tarball. Either may be present alone, both, or neither."""

    vm: Optional[SnapshotVM] = None
    """microVM-state snapshot (a no-op for the container backend)."""
    files: Optional[SnapshotFiles] = None
    """Writable-filesystem snapshot."""


class SnapshotPartResult(BaseModel):
    """Outcome of capturing one snapshot part."""

    captured: bool
    """Whether this part was captured. False (with ``reason``) when unsupported on the active backend, e.g. ``vm`` on a container."""
    key: str
    """Key the part was written under."""
    bytes: Optional[int] = None
    """Size of the captured artifact in bytes, when known."""
    reason: Optional[str] = None
    """Why the part was not captured, when ``captured`` is false."""


class SnapshotResult(BaseModel):
    """Outcome of a snapshot action, reported independently for each requested part."""

    vm: Optional[SnapshotPartResult] = None
    files: Optional[SnapshotPartResult] = None


class SandboxConfig(BaseModel):
    """Hive sandbox configuration."""

    image: Optional[str] = None
    """Reference to the agent image to launch. Cannot be changed after the sandbox is initialized."""
    cpu: Optional[float] = Field(None, gt=0)
    """Number of virtual CPUs allocated to the sandbox, as a ceiling (the pod CPU limit). May be fractional (e.g. 0.5); the microvm guest vCPU count is this value rounded up. Defaults to 1. Cannot be changed after the sandbox is initialized."""
    request_cpu: Optional[float] = Field(None, gt=0)
    """CPU cores reserved for the sandbox at schedule time (the pod CPU request), decoupled from cpu (the limit) so an idle sandbox reserves less than it can burst to. Defaults to 0.5. Cannot be changed after the sandbox is initialized."""
    memory: Optional[int] = Field(None, ge=128)
    """Memory allocated to the sandbox, in MiB. Defaults to 512. Cannot be changed after the sandbox is initialized."""
    entrypoint: Optional[Union[str, list[str]]] = None
    """Override the entrypoint used when the sandbox is run. Accepts either an argv list (each element a separate argument) or a single string, which the sandbox splits on whitespace into arguments. When omitted, the image's default entrypoint is used."""
    cwd: Optional[str] = None
    """Working directory for the entrypoint. When omitted, the image's working directory is used. Cannot be changed after the sandbox is initialized."""
    tty: Optional[bool] = None
    """When true, the entrypoint is launched attached to a pseudo-TTY; attach to it by calling ``exec_stream`` with an empty command. Container isolation only. Defaults to false. Cannot be changed after the sandbox is initialized."""
    env: Optional[dict[str, str]] = None
    """Additional environment variables in ``KEY=VALUE`` form. Cannot be changed after the sandbox is initialized."""
    extra_hosts: Optional[list[str]] = None
    """Additional ``/etc/hosts`` entries in ``hostname:ip`` form (use ``host-gateway`` for the host machine's IP). Cannot be changed after the sandbox is initialized."""
    ttl: Optional[int] = Field(None, ge=0)
    """Sandbox time to live in seconds. Call :meth:`Sandbox.ping` to reset the timer; once a ping has not been received for this long the sandbox is stopped. Defaults to 1800 (30 min). Use ``0`` to disable shutdown."""
    fs: Optional[list[FileSystem]] = Field(None, min_length=1)
    """File systems exposed to the agent. Mount paths must be unique and non-overlapping (a mount path may not be a parent directory of another)."""
    egress: Optional[list[EgressRule]] = None
    """Ordered list of egress rules. The first rule that matches a request decides the outcome; requests that match no rule are denied."""
    snapshot: Optional[Snapshot] = None
    """Snapshot configuration for this sandbox."""


class SandboxInfo(BaseModel):
    """Internal runtime information about a sandbox, determined at boot rather than configured."""

    isolation: Literal["container", "microvm"]
    """The isolation mechanism the sandbox is running with. Selected automatically from the image (a microvm image ships a guest root filesystem), not configured."""


class _FSChanges(BaseModel):
    added: Optional[list[FileSystem]] = None
    removed: Optional[list[FileSystem]] = None


class _EgressChanges(BaseModel):
    added: Optional[list[EgressRule]] = None
    removed: Optional[list[EgressRule]] = None


class Changes(BaseModel):
    """Concrete additions and removals carried out by an apply call. Each list contains whole entries so the caller can audit what changed without re-diffing the request."""

    fs: Optional[_FSChanges] = None
    """File systems added or removed."""
    egress: Optional[_EgressChanges] = None
    """Egress rules added or removed."""
    warnings: Optional[list[str]] = None
    """Non-fatal advisories. Example: a non-modifiable field was present in the request and was ignored."""


class ApplyResult(BaseModel):
    """The outcome of an apply call."""

    applied: bool
    """``True`` if every change was applied successfully. ``False`` if the apply failed and was rolled back; in that case the sandbox is unchanged."""
    config: SandboxConfig
    """The configuration in effect after this call."""
    changes: Changes
    """What was added or removed by this call."""
    error: Optional[str] = None
    """Human-readable failure reason. Set only when ``applied`` is ``False``."""


class ApiError(BaseModel):
    """Structured error body returned by the server."""

    error: str
    """Human-readable failure reason."""
    details: Optional[dict[str, object]] = None
    """Optional structured context such as the offending field path or a conflict identifier."""


class SandboxRef(BaseModel):
    """A provisioned sandbox handle returned by the controller."""

    id: str
    """Server-assigned unique identifier (uuid)."""
    key: str
    """Caller-chosen key the sandbox was provisioned under; used for routing."""



class _SandboxEventBase(BaseModel):
    id: int
    """Monotonic event id, usable as a resume cursor."""
    timestamp: str
    """When the event occurred, as an ISO-8601 string."""


class ConfigApplyEvent(_SandboxEventBase):
    """Emitted when a configuration apply is attempted."""

    type: Literal["config.apply"]
    success: bool
    changes: Changes
    error_message: Optional[str] = Field(None, alias="errorMessage")

    model_config = ConfigDict(populate_by_name=True)


class EgressRequestEvent(_SandboxEventBase):
    """An outbound request the agent made through the egress proxy."""

    type: Literal["egress.request"]
    access: Literal["allowed", "denied"]
    host: str
    method: str
    path: str
    query: Optional[str] = None
    headers: Optional[dict[str, str]] = None
    body: Optional[str] = None


class EgressResponseEvent(_SandboxEventBase):
    """The response to an earlier egress request."""

    type: Literal["egress.response"]
    request_id: int
    status: int
    duration_ms: int
    headers: Optional[dict[str, str]] = None


class EgressChunkEvent(_SandboxEventBase):
    """A streamed body chunk for an egress request or response."""

    type: Literal["egress.chunk"]
    request_id: int
    body: str
    label: Optional[str] = None
    """Optional origin tag: ``up`` for client→upstream, ``down`` for upstream→client (WebSocket only)."""


class FSRequestEvent(_SandboxEventBase):
    """A file operation the agent attempted against a mount."""

    type: Literal["fs.request"]
    access: Literal["allowed", "denied"]
    mount: str
    path: str
    operation: Literal["read", "write"]


class FSResponseEvent(_SandboxEventBase):
    """The outcome of an earlier file operation."""

    type: Literal["fs.response"]
    backend: Literal["local", "gdrive", "gcs", "s3", "azure", "onedrive", "external"]
    request_id: int
    duration_ms: int
    error: Optional[str] = None


class StdioEvent(_SandboxEventBase):
    """Standard output or error emitted by the sandbox entrypoint."""

    type: Literal["stdio"]
    stdout: Optional[str] = None
    stderr: Optional[str] = None


class ResourceUsageEvent(_SandboxEventBase):
    """A periodic sample of the sandbox's CPU and memory usage."""

    type: Literal["resource.usage"]
    cpu_percent: float
    memory_bytes: int


class ExecRequestEvent(_SandboxEventBase):
    """A command started inside the sandbox."""

    type: Literal["exec.request"]
    cwd: str
    command: str


class ExecResponseEvent(_SandboxEventBase):
    """Marks completion of an earlier exec request."""

    type: Literal["exec.response"]
    request_id: int


class IngressRequestEvent(_SandboxEventBase):
    """An inbound request reaching a sandbox port through the proxy."""

    type: Literal["ingress.request"]
    port: str
    method: str
    path: str
    query: Optional[str] = None
    headers: Optional[dict[str, str]] = None
    body: Optional[str] = None


class IngressResponseEvent(_SandboxEventBase):
    """The response to an earlier ingress request."""

    type: Literal["ingress.response"]
    request_id: int
    status: int
    duration_ms: int
    headers: Optional[dict[str, str]] = None
    body: Optional[str] = None


class SystemEvent(_SandboxEventBase):
    """A sandbox lifecycle transition: ``system.start`` when the request to start
    the VM or container is first received, ``system.config-changed`` when the
    config is updated (``config`` carries the new config), and ``system.shutdown``
    when the sandbox expires its TTL without activity."""

    type: Literal["system.start", "system.config-changed", "system.shutdown"]
    config: Optional[SandboxConfig] = None


SandboxEvent = Annotated[
    Union[
        ConfigApplyEvent,
        EgressRequestEvent,
        EgressResponseEvent,
        EgressChunkEvent,
        FSRequestEvent,
        FSResponseEvent,
        StdioEvent,
        ResourceUsageEvent,
        ExecRequestEvent,
        ExecResponseEvent,
        IngressRequestEvent,
        IngressResponseEvent,
        SystemEvent,
    ],
    Field(discriminator="type"),
]
"""A single activity event from a sandbox. Inspect ``type`` to discriminate the variant."""

FileSystemAdapter: TypeAdapter[FileSystem] = TypeAdapter(FileSystem)
SandboxEventAdapter: TypeAdapter[SandboxEvent] = TypeAdapter(SandboxEvent)
