import { useRef, useState } from "react";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Label } from "@/components/ui/label";
import { useTransport, type TraceData } from "@/lib/transport";

const SPEEDS = [1, 5, 10, 100] as const;

interface Props {
  open: boolean;
  onOpenChange: (open: boolean) => void;
}

export function TraceDialog({ open, onOpenChange }: Props) {
  const { loadTraceFromData, clearTrace, player, playbackSpeed, setPlaybackSpeed } = useTransport();
  const [error, setError] = useState<string | null>(null);
  const inputRef = useRef<HTMLInputElement>(null);

  function handleFile(e: React.ChangeEvent<HTMLInputElement>) {
    const file = e.target.files?.[0];
    if (!file) return;
    setError(null);
    file.text().then((text) => {
      try {
        const data = JSON.parse(text) as TraceData;
        loadTraceFromData(data);
        onOpenChange(false);
      } catch {
        setError("Failed to parse trace file — must be valid JSON.");
      }
    }).catch(() => setError("Could not read file."));
  }

  function handleStop() {
    clearTrace();
    onOpenChange(false);
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-sm">
        <DialogHeader>
          <DialogTitle>Load Trace</DialogTitle>
        </DialogHeader>

        <div className="grid gap-4 py-2">
          {player && (
            <div className="flex items-center justify-between rounded-md border border-green-700/30 bg-green-600/10 dark:border-green-500/30 dark:bg-green-500/10 px-3 py-2">
              <span className="text-xs text-green-700 dark:text-green-400">Replaying — {playbackSpeed}×</span>
              <Button variant="ghost" size="sm" onClick={handleStop}>
                Stop
              </Button>
            </div>
          )}

          <div className="grid gap-1.5">
            <Label>Playback speed</Label>
            <div className="flex gap-2">
              {SPEEDS.map((s) => (
                <Button
                  key={s}
                  variant={playbackSpeed === s ? "secondary" : "outline"}
                  size="sm"
                  onClick={() => setPlaybackSpeed(s)}
                  className="w-14"
                >
                  {s}×
                </Button>
              ))}
            </div>
          </div>

          <div className="grid gap-1.5">
            <Label>Trace file</Label>
            <Button variant="outline" onClick={() => inputRef.current?.click()}>
              Browse…
            </Button>
            <input
              ref={inputRef}
              type="file"
              accept=".json,application/json"
              className="hidden"
              onChange={handleFile}
            />
          </div>

          {error && <p className="text-xs text-red-600 dark:text-red-400">{error}</p>}
        </div>
      </DialogContent>
    </Dialog>
  );
}
