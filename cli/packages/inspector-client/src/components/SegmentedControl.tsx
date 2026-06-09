import { cn } from "@/lib/utils";

interface Props<T extends string> {
  options: { value: T; label: string }[];
  value: T;
  onChange: (value: T) => void;
}

export function SegmentedControl<T extends string>({
  options,
  value,
  onChange,
}: Props<T>) {
  return (
    <div className="flex rounded-md border border-border overflow-hidden">
      {options.map((opt) => (
        <button
          key={opt.value}
          onClick={() => onChange(opt.value)}
          className={cn(
            "px-2.5 py-0.5 text-xs transition-colors",
            value === opt.value
              ? "bg-accent text-accent-foreground"
              : "hover:bg-muted/50 text-muted-foreground",
          )}
        >
          {opt.label}
        </button>
      ))}
    </div>
  );
}
