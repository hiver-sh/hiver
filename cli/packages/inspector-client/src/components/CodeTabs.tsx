import { useState } from "react";
import { CodeViewer } from "@/components/CodeViewer";
import { LangIcon } from "@/components/LangIcon";
import { cn } from "@/lib/utils";

export type CodeLang = "ts" | "py" | "go";

const LANGS: { id: CodeLang; icon: string; viewer: string }[] = [
  { id: "ts", icon: "typescript", viewer: "typescript" },
  { id: "py", icon: "python", viewer: "python" },
  { id: "go", icon: "go", viewer: "go" },
];

export interface CodeTabsProps {
  /** Code per language; only languages present are shown as tabs, in ts/py/go order. */
  examples: Partial<Record<CodeLang, string>>;
  className?: string;
}

/**
 * Language-tabbed code snippet — the LangIcon segmented control over a
 * read-only CodeViewer, matching the "Get started" panel on the home view.
 */
export function CodeTabs({ examples, className }: CodeTabsProps) {
  const tabs = LANGS.filter((t) => examples[t.id] !== undefined);
  const [lang, setLang] = useState<CodeLang>(tabs[0]?.id ?? "ts");
  const active = tabs.find((t) => t.id === lang) ?? tabs[0];

  return (
    <div className={cn("flex flex-col gap-2", className)}>
      <div className="flex gap-0.5 self-start rounded-md border border-border p-0.5 text-xs">
        {tabs.map(({ id, icon }) => (
          <button
            key={id}
            onClick={() => setLang(id)}
            className={cn(
              "flex items-center gap-1.5 rounded px-2.5 py-1 transition-colors",
              lang === id
                ? "bg-muted text-foreground"
                : "text-muted-foreground hover:text-foreground",
            )}
          >
            <LangIcon lang={icon} className="h-3.5 w-3.5" />
          </button>
        ))}
      </div>
      <div className="overflow-hidden rounded-lg border border-border">
        <CodeViewer
          content={examples[active.id]!}
          lang={active.viewer}
          autoSize
        />
      </div>
    </div>
  );
}
