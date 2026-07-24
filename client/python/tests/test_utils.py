from hiver.schemas import SandboxConfig, Snapshot, SnapshotVM
from hiver.utils import allow_sandbox


def _pinned_bodies(rules):
    return [
        r.override.body
        for r in rules
        if r.override is not None and r.override.body_strategy == "replace"
    ]


# The pinned body replaces the nested create's request body verbatim, skipping
# get_or_create_sandbox's client-side defaulting. Without the same defaults a
# config with no fs creates a sandbox with NO workspace — while a resumed VM
# snapshot still holds its 9p /workspace mount, which is then orphaned and
# wedges the first process to touch it in p9_client_rpc.
def test_allow_sandbox_pins_default_workspace_fs() -> None:
    rules = allow_sandbox(
        "worker-1",
        SandboxConfig(image="browser", snapshot=Snapshot(vm=SnapshotVM(key="browser"))),
    )
    bodies = _pinned_bodies(rules)
    assert len(bodies) == 2  # docker + k8s gateway hosts
    for body in bodies:
        assert body["fs"] == [
            {
                "backend": "local",
                "mount": "/workspace",
                "acls": [{"path": "/workspace/**", "access": "rw"}],
            }
        ]
        assert body["image"] == "browser"
        assert body["snapshot"] == {"vm": {"key": "browser"}}


def test_allow_sandbox_keeps_explicit_fs() -> None:
    config = SandboxConfig.model_validate(
        {"fs": [{"backend": "local", "mount": "/data"}]}
    )
    for body in _pinned_bodies(allow_sandbox("worker-1", config)):
        assert [f["mount"] for f in body["fs"]] == ["/data"]
