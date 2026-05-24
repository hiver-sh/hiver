import { useState } from "react";
import { Check, Clipboard } from "lucide-react";
import MonacoEditor, { loader } from "@monaco-editor/react";
import type { editor } from "monaco-editor";
import * as monaco from "monaco-editor";
import { SANDBOX_CONFIG_SCHEMA } from "@/sandboxConfigSchema";
import { MONACO_THEME } from "@/monacoWorkers";

loader.config({ monaco });

const SCHEMA_URI = "https://hive.sandbox/config-schema.json";

interface Props {
  value: string;
  onChange: (value: string) => void;
  className?: string;
}

function configureJsonSchema(m: typeof monaco) {
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  (m.languages as any).json.jsonDefaults.setDiagnosticsOptions({
    validate: true,
    schemaValidation: "error",
    schemas: [{ uri: SCHEMA_URI, fileMatch: ["*"], schema: SANDBOX_CONFIG_SCHEMA }],
  });
}

export function CodeEditor({ value, onChange, className }: Props) {
  const [copied, setCopied] = useState(false);

  function handleMount(_ed: editor.IStandaloneCodeEditor, m: typeof monaco) {
    configureJsonSchema(m);
  }

  function handleCopy() {
    navigator.clipboard.writeText(value);
    setCopied(true);
    setTimeout(() => setCopied(false), 2000);
  }

  return (
    <div className={`monaco-bg relative overflow-hidden rounded-md border border-input ${className ?? ""}`}>
      <MonacoEditor
        height="100%"
        defaultLanguage="json"
        value={value}
        onChange={(v: string | undefined) => onChange(v ?? "")}
        theme={MONACO_THEME}
        onMount={handleMount}
        options={{
          minimap: { enabled: false },
          lineNumbers: "on",
          scrollBeyondLastLine: false,
          wordWrap: "on",
          tabSize: 2,
          formatOnPaste: true,
          automaticLayout: true,
          fontSize: 13,
          padding: { top: 8, bottom: 8 },
        }}
      />
      <button
        onClick={handleCopy}
        className="absolute top-2 right-3 p-1.5 rounded-md bg-muted/60 hover:bg-muted text-muted-foreground hover:text-foreground transition-colors z-10"
        title="Copy to clipboard"
      >
        {copied ? <Check className="h-3.5 w-3.5" /> : <Clipboard className="h-3.5 w-3.5" />}
      </button>
    </div>
  );
}
