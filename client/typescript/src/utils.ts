import { EgressRule } from "./schemas";

export function allowedPythonPackages(...packages: string[]): EgressRule[] {
  return [
    {
      access: "allow",
      host: "pypi.org",
      paths: packages.map((pyPackage) => `/simple/${pyPackage}/`),
    },
    {
      access: "allow",
      host: "files.pythonhosted.org",
    },
  ];
}

export function allowedNpmPackages(...packages: string[]): EgressRule[] {
  return packages.map((packageName: string) => {
    return {
      access: "allow",
      host: "registry.npmjs.org",
      paths: [`/${packageName}`, `/${packageName}/*`],
    };
  });
}
