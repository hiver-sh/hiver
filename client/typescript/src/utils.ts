import { EgressRule } from "./schemas";

export function allowedPythonPackages(...packages: string[]) : EgressRule[] {
  return [
    {
        host: "pypi.org",
        paths: packages.map((pyPackage) => `/simple/${pyPackage}/`),
    },
    {
        host: "files.pythonhosted.org",
    }
  ];
}

export function allowedNpmPackages(...packages: string[]): EgressRule[] {
    return packages.map((packageName: string) => {
        return {
            host: "registry.npmjs.org",
            paths: [`/${packageName}`, `/${packageName}/*`]

        };

    });
}
