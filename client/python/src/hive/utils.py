from .schemas import EgressRule


def allowed_python_packages(*packages: str) -> list[EgressRule]:
    return [
        EgressRule(
            host="pypi.org",
            paths=[f"/simple/{pkg}/" for pkg in packages],
        ),
        EgressRule(host="files.pythonhosted.org"),
    ]


def allowed_npm_packages(*packages: str) -> list[EgressRule]:
    return [
        EgressRule(
            host="registry.npmjs.org",
            paths=[f"/{pkg}", f"/{pkg}/*"],
        )
        for pkg in packages
    ]
