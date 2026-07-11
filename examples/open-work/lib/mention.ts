// `@`-mentions reference workspace files by path. Paths with spaces or other
// characters that would terminate the token are wrapped in double quotes, e.g.
// `@"input/Ariana and Emmanuel- 0008.jpg"`.

/** A path needs quoting if it contains anything outside a bare token charset. */
export function needsQuoting(path: string): boolean {
  return /[^A-Za-z0-9._/-]/.test(path);
}

/** Render a file path as an `@`-mention, quoting the path when needed. */
export function mention(path: string): string {
  return needsQuoting(path) ? `@"${path}"` : `@${path}`;
}

/** Fresh matcher for `@path` or `@"path with spaces"` (path = group 1 or 2). */
export function mentionRegex(): RegExp {
  return /@(?:"([^"]+)"|([^\s@"]+))/g;
}

/** The referenced path from a {@link mentionRegex} match (unquoted). */
export function mentionPath(m: RegExpMatchArray): string {
  return m[1] ?? m[2];
}
