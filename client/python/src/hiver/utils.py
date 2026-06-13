from .schemas import EgressRule


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
