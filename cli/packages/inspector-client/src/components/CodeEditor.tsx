import { useRef, useState } from "react";
import { Check, Clipboard } from "lucide-react";
import MonacoEditor, { loader } from "@monaco-editor/react";
import type { editor } from "monaco-editor";
import monaco, { jsonDefaults } from "@/lib/monaco";
import { SANDBOX_CONFIG_SCHEMA, SNAPSHOT_SCHEMA } from "@/sandboxConfigSchema";
import { useMonacoTheme } from "@/lib/useMonacoTheme";

loader.config({ monaco });

// Each schema is scoped to a distinct model path (via fileMatch) so an editor
// validates against only the schema for its kind — the snapshot editor must not
// be flagged against the full SandboxConfig schema, and vice versa.
type SchemaKind = "config" | "snapshot";

const SCHEMAS: Record<
  SchemaKind,
  { uri: string; path: string; schema: object }
> = {
  config: {
    uri: "https://hive.sandbox/config-schema.json",
    path: "sandbox-config.json",
    schema: SANDBOX_CONFIG_SCHEMA,
  },
  snapshot: {
    uri: "https://hive.sandbox/snapshot-schema.json",
    path: "snapshot-config.json",
    schema: SNAPSHOT_SCHEMA,
  },
};

interface Props {
  value: string;
  onChange: (value: string) => void;
  className?: string;
  /** Which schema the editor validates against. Defaults to the full config. */
  schema?: SchemaKind;
  /** Grows the editor to fit its content instead of filling the parent. */
  autoSize?: boolean;
}

function configureJsonSchema() {
  // The ESM monaco build exposes jsonDefaults as a module export rather than on
  // monaco.languages.json (which only exists in the AMD/barrel build).
  jsonDefaults.setDiagnosticsOptions({
    validate: true,
    schemaValidation: "error",
    schemas: Object.values(SCHEMAS).map((s) => ({
      uri: s.uri,
      fileMatch: [s.path],
      schema: s.schema,
    })),
  });
}

export function CodeEditor({
  value,
  onChange,
  className,
  schema = "config",
  autoSize,
}: Props) {
  const [copied, setCopied] = useState(false);
  const [focused, setFocused] = useState(false);
  const containerRef = useRef<HTMLDivElement>(null);
  const monacoTheme = useMonacoTheme();

  function handleMount(ed: editor.IStandaloneCodeEditor) {
    configureJsonSchema();
    ed.onDidFocusEditorWidget(() => setFocused(true));
    ed.onDidBlurEditorWidget(() => setFocused(false));
    if (autoSize) {
      // Track content height so the editor grows/shrinks with its text rather
      // than scrolling inside a fixed box.
      const update = () => {
        const h = ed.getContentHeight();
        if (containerRef.current) containerRef.current.style.height = `${h}px`;
        ed.layout();
      };
      ed.onDidContentSizeChange(update);
      update();
    }
  }

  function handleCopy() {
    navigator.clipboard.writeText(value);
    setCopied(true);
    setTimeout(() => setCopied(false), 2000);
  }

  return (
    <div
      ref={containerRef}
      className={`relative overflow-hidden rounded-md border border-input ring-offset-background ${focused ? "ring-2 ring-ring ring-offset-2" : ""} ${className ?? ""}`}
    >
      <MonacoEditor
        height="100%"
        defaultLanguage="json"
        path={SCHEMAS[schema].path}
        value={value}
        onChange={(v: string | undefined) => onChange(v ?? "")}
        theme={monacoTheme}
        onMount={handleMount}
        options={{
          minimap: { enabled: false },
          lineNumbers: "off",
          scrollBeyondLastLine: false,
          wordWrap: "on",
          tabSize: 2,
          formatOnPaste: true,
          automaticLayout: true,
          fontSize: 13,
          padding: { top: 8, bottom: 8 },
          folding: false,
          inlayHints: { enabled: "off" },
          breadcrumbs: { enabled: false },
        }}
      />
      <button
        onClick={handleCopy}
        className="absolute top-2 right-3 p-1.5 rounded-md bg-muted/60 hover:bg-muted text-muted-foreground hover:text-foreground transition-colors z-10"
        title="Copy to clipboard"
      >
        {copied ? (
          <Check className="h-3.5 w-3.5" />
        ) : (
          <Clipboard className="h-3.5 w-3.5" />
        )}
      </button>
    </div>
  );
}
