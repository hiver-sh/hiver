/**
 * Formats a request/response body for display in a CodeViewer, and decides
 * whether it should be highlighted as JSON.
 *
 * Valid JSON is re-prettified. Content that fails strict `JSON.parse` but
 * clearly looks like JSON — e.g. payloads with unescaped control characters in
 * string values, which `JSON.parse` rejects but are common in real traffic — is
 * still flagged as `json` so monaco colorizes it (its tokenizer is lenient even
 * where `JSON.parse` is not); it's just shown as-is without reformatting.
 */
export function tryPretty(
  body?: string,
): { content: string; isJson: boolean } | undefined {
  if (!body) return undefined;
  try {
    return { content: JSON.stringify(JSON.parse(body), null, 2), isJson: true };
  } catch {
    const t = body.trimStart();
    const looksJson = t.startsWith("{") || t.startsWith("[");
    return { content: body, isJson: looksJson };
  }
}
