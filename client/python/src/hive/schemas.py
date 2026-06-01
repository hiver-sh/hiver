from __future__ import annotations

from typing import Annotated, Literal, Optional, Union
from pydantic import BaseModel, ConfigDict, Field, TypeAdapter, field_validator


class ACLRule(BaseModel):
    path: str
    access: Literal["rw", "ro", "deny"]


class _FileSystemBase(BaseModel):
    mount: str
    acls: Optional[list[ACLRule]] = None

    @field_validator("mount")
    @classmethod
    def _mount_must_be_absolute(cls, v: str) -> str:
        if not v.startswith("/"):
            raise ValueError("mount must be an absolute path")
        return v


class LocalFileSystem(_FileSystemBase):
    backend: Literal["local"]
    origin: Optional[str] = None


class GDriveFileSystem(_FileSystemBase):
    backend: Literal["gdrive"]
    gdrive_access_token: Optional[str] = None
    gdrive_refresh_token: Optional[str] = None
    gdrive_client_id: Optional[str] = None
    gdrive_client_secret: Optional[str] = None
    gdrive_service_account_json: Optional[str] = None
    gdrive_folder_id: Optional[str] = None


class GCSFileSystem(_FileSystemBase):
    backend: Literal["gcs"]
    gcs_bucket: str
    gcs_prefix: Optional[str] = None
    gcs_service_account_json: str


FileSystem = Annotated[
    Union[LocalFileSystem, GDriveFileSystem, GCSFileSystem],
    Field(discriminator="backend"),
]

HttpMethod = Literal["GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"]


class EgressOverride(BaseModel):
    query: Optional[dict[str, str]] = None
    headers: Optional[dict[str, str]] = None


class EgressRule(BaseModel):
    access: Literal["allow", "deny"]
    host: str
    ports: Optional[list[int]] = None
    methods: Optional[list[HttpMethod]] = None
    paths: Optional[list[str]] = None
    override: Optional[EgressOverride] = None


class Snapshot(BaseModel):
    restore_key: Optional[str] = Field(None, pattern=r'^[A-Za-z0-9_-]{1,64}$')
    write_key: Optional[str] = Field(None, pattern=r'^[A-Za-z0-9_-]{1,64}$')
    include: Optional[list[str]] = Field(None, min_length=1)


class SandboxConfig(BaseModel):
    image: Optional[str] = None
    entrypoint: Optional[str] = None
    env: Optional[dict[str, str]] = None
    ttl: Optional[int] = Field(None, ge=0)
    fs: list[FileSystem] = Field(min_length=1)
    egress: Optional[list[EgressRule]] = None
    snapshot: Optional[Snapshot] = None


class _FSChanges(BaseModel):
    added: Optional[list[FileSystem]] = None
    removed: Optional[list[FileSystem]] = None


class _EgressChanges(BaseModel):
    added: Optional[list[EgressRule]] = None
    removed: Optional[list[EgressRule]] = None


class Changes(BaseModel):
    fs: Optional[_FSChanges] = None
    egress: Optional[_EgressChanges] = None
    warnings: Optional[list[str]] = None


class ApplyResult(BaseModel):
    applied: bool
    config: SandboxConfig
    changes: Changes
    error: Optional[str] = None


class ApiError(BaseModel):
    error: str
    details: Optional[dict[str, object]] = None


class SandboxRef(BaseModel):
    id: str
    endpoint: str
    exposed_endpoint: Optional[str] = None


class SandboxDetail(SandboxRef):
    """Extended sandbox record returned by GET /v1/sandboxes/{id}."""
    terminal_cmd: Optional[str] = None


class _SandboxEventBase(BaseModel):
    id: int
    timestamp: str


class ConfigApplyEvent(_SandboxEventBase):
    type: Literal["config.apply"]
    success: bool
    changes: Changes
    error_message: Optional[str] = Field(None, alias="errorMessage")

    model_config = ConfigDict(populate_by_name=True)


class EgressRequestEvent(_SandboxEventBase):
    type: Literal["egress.request"]
    access: Literal["allowed", "denied"]
    host: str
    method: str
    path: str
    query: Optional[str] = None
    headers: Optional[dict[str, str]] = None
    body: Optional[str] = None


class EgressResponseEvent(_SandboxEventBase):
    type: Literal["egress.response"]
    request_id: int
    status: int
    duration_ms: int
    headers: Optional[dict[str, str]] = None


class EgressChunkEvent(_SandboxEventBase):
    type: Literal["egress.chunk"]
    request_id: int
    body: str
    # `up` for client→upstream, `down` for upstream→client (WebSocket only).
    label: Optional[str] = None


class FSRequestEvent(_SandboxEventBase):
    type: Literal["fs.request"]
    access: Literal["allowed", "denied"]
    mount: str
    path: str
    operation: Literal["read", "write"]


class FSResponseEvent(_SandboxEventBase):
    type: Literal["fs.response"]
    backend: Literal["local", "gdrive", "gcs"]
    request_id: int
    duration_ms: int
    error: Optional[str] = None


class StdioEvent(_SandboxEventBase):
    type: Literal["stdio"]
    stdout: Optional[str] = None
    stderr: Optional[str] = None


class ResourceUsageEvent(_SandboxEventBase):
    type: Literal["resource.usage"]
    cpu_percent: float
    memory_bytes: int


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
    ],
    Field(discriminator="type"),
]

FileSystemAdapter: TypeAdapter[FileSystem] = TypeAdapter(FileSystem)
SandboxEventAdapter: TypeAdapter[SandboxEvent] = TypeAdapter(SandboxEvent)
