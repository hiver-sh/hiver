from .controller import sandbox_config_with_defaults
from .schemas import EgressOverride, EgressRule, SandboxConfig


def allowed_python_packages(*packages: str) -> list[EgressRule]:
    """Build egress rules that allow installing the named packages from PyPI.

    Add the returned rules to ``SandboxConfig.egress`` so ``pip install`` can
    reach only those packages.
    """
    return [
        EgressRule(
            access="allow",
            host="pypi.org",
            paths=[f"/simple/{pkg}/" for pkg in packages],
        ),
        EgressRule(access="allow", host="files.pythonhosted.org"),
    ]


def allowed_npm_packages(*packages: str) -> list[EgressRule]:
    """Build egress rules that allow installing the named packages from the npm registry.

    Add the returned rules to ``SandboxConfig.egress`` so ``npm install`` can
    reach only those packages.
    """
    return [
        EgressRule(
            access="allow",
            host="registry.npmjs.org",
            paths=[f"/{pkg}", f"/{pkg}/*"],
        )
        for pkg in packages
    ]


def allow_sandbox(
    sandbox_key: str,
    config: SandboxConfig,
    allowed_dirs: list[str] | None = None,
) -> list[EgressRule]:
    """Build egress rules that let an agent create and reach a single nested sandbox.

    The agent may POST to create the sandbox named ``sandbox_key`` through the
    gateway, but its request body is replaced with ``config`` so the agent
    cannot influence what gets created. A second passthrough rule allows the
    sandbox's gateway proxy routes. Both the Docker (``gateway``) and k8s
    (``gateway.hiver``) gateway hosts are covered.

    Pass ``allowed_dirs`` to also open the nested sandbox's file API under
    specific directories. Each entry allows ``POST``/``GET``/``DELETE`` on the
    file endpoint's ``.../file/<dir>/**`` glob, so the agent can seed and read
    back files there without reaching the rest of the sandbox's filesystem. When
    omitted, no file rules are added. Entries are matched relative to the file
    endpoint; a leading slash is stripped, so both ``"workspace/inputs"`` and
    ``"/workspace/inputs"`` work.

    Add the returned rules to the outer ``SandboxConfig.egress``.
    """
    # The pinned body replaces the nested create's body verbatim, bypassing
    # get_or_create_sandbox's defaulting — apply the same defaults here so the
    # nested sandbox comes up exactly as a direct create with ``config`` would.
    body = sandbox_config_with_defaults(config).model_dump(exclude_none=True)
    rules: list[EgressRule] = []
    for host in ("gateway", "gateway.hiver"):
        rules.extend(
            [
                EgressRule(
                    access="allow",
                    host=host,
                    paths=[f"/v1/sandboxes/{sandbox_key}"],
                    methods=["POST"],
                    override=EgressOverride(
                        body=body,
                        body_strategy="replace",
                    ),
                ),
                EgressRule(
                    access="allow",
                    host=host,
                    paths=[f"/sandbox/*/v1/{sandbox_key}/proxy/**"],
                ),
            ]
        )
        if allowed_dirs:
            rules.append(
                EgressRule(
                    access="allow",
                    host=host,
                    paths=[
                        f"/sandbox/*/v1/{sandbox_key}/file/{dir.removeprefix('/')}/**"
                        for dir in allowed_dirs
                    ],
                )
            )
    return rules
