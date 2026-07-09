import { useCallback, useEffect, useRef, useState } from "react";
import { Pause, Play, RotateCcw } from "lucide-react";
import { cn } from "@/lib/utils";
import { useTransport } from "@/lib/transport";

// Speeds offered by the segmented control. Mirror the trace player's `speed`.
const SPEED_OPTIONS = [0.5, 1, 2, 4];

// m:ss playhead clock. humanDuration ("1.2s") reads well for bar durations but
// not for a scrubbing readout, which wants a stable, monospace-friendly clock.
function formatClock(ms: number): string {
  if (!Number.isFinite(ms) || ms < 0) ms = 0;
  const totalSec = Math.floor(ms / 1000);
  const m = Math.floor(totalSec / 60);
  const s = totalSec % 60;
  return `${m}:${s.toString().padStart(2, "0")}`;
}

// Transport bar for trace replay: play/pause, a seekable progress scrubber, an
// elapsed/total clock, and a speed selector. Renders nothing outside replay
// (no player) so the caller can mount it unconditionally.
export function PlaybackControls() {
  const { player, paused, togglePaused, seek, playbackSpeed, setPlaybackSpeed } =
    useTransport();

  // Re-render on discrete player changes (pause/resume/seek/speed) and as a
  // streaming trace fills in (duration grows). The continuously-moving playhead
  // is driven by the rAF loop below, not this subscription.
  const [, force] = useState(0);

  // While dragging the scrubber, preview the position locally and commit the
  // seek only on release — a live backward drag would reset and re-pump the
  // whole feed on every pixel of travel.
  const trackRef = useRef<HTMLDivElement>(null);
  const [dragMs, setDragMs] = useState<number | null>(null);

  const duration = player?.durationMs ?? 0;
  const rawElapsed = player ? player.elapsedReplayMs : 0;
  const displayMs = dragMs !== null ? dragMs : Math.min(rawElapsed, duration);
  const frac = duration > 0 ? Math.min(1, Math.max(0, displayMs / duration)) : 0;
  // "Ended" only once a fully-loaded trace's clock has passed its last record
  // and the user isn't mid-scrub. Turns the play button into a replay-from-start.
  const ended =
    !!player &&
    dragMs === null &&
    player.loadComplete &&
    duration > 0 &&
    rawElapsed >= duration;

  useEffect(() => {
    if (!player) return;
    return player.subscribe(() => force((n) => n + 1));
  }, [player]);

  // Advance the displayed playhead ~once per frame while playing. Stops when
  // paused or once the clock has run past the end of a fully-loaded trace (a
  // dangling recording would otherwise tick forever). `ended` is a dependency so
  // a seek back from the end (ended: true→false) restarts the loop — otherwise
  // the playhead would stay frozen even though the clock is running again.
  useEffect(() => {
    if (!player || paused || ended) return;
    let raf = 0;
    const loop = () => {
      force((n) => n + 1);
      const atEnd =
        player.loadComplete &&
        player.durationMs > 0 &&
        player.elapsedReplayMs >= player.durationMs;
      if (!atEnd) raf = requestAnimationFrame(loop);
    };
    raf = requestAnimationFrame(loop);
    return () => cancelAnimationFrame(raf);
  }, [player, paused, ended]);

  const msFromClientX = useCallback(
    (clientX: number): number => {
      const el = trackRef.current;
      if (!el || duration <= 0) return 0;
      const rect = el.getBoundingClientRect();
      const f = (clientX - rect.left) / rect.width;
      return Math.min(1, Math.max(0, f)) * duration;
    },
    [duration],
  );

  const onPointerDown = useCallback(
    (e: React.PointerEvent) => {
      if (duration <= 0) return;
      e.currentTarget.setPointerCapture(e.pointerId);
      setDragMs(msFromClientX(e.clientX));
    },
    [duration, msFromClientX],
  );
  const onPointerMove = useCallback(
    (e: React.PointerEvent) => {
      setDragMs((prev) => (prev === null ? prev : msFromClientX(e.clientX)));
    },
    [msFromClientX],
  );
  const onPointerUp = useCallback(
    (e: React.PointerEvent) => {
      // Commit the scrub in the event phase — NOT inside a setDragMs updater,
      // which runs during render and would make seek()'s provider setState a
      // "setState while rendering another component" violation.
      if (dragMs === null) return;
      if (e.currentTarget.hasPointerCapture(e.pointerId))
        e.currentTarget.releasePointerCapture(e.pointerId);
      seek(dragMs);
      setDragMs(null);
    },
    [dragMs, seek],
  );

  if (!player) return null;

  const onPlayPause = () => {
    if (ended) {
      // Rewind to the start and (re)start playing.
      seek(0);
      if (paused) togglePaused();
      return;
    }
    togglePaused();
  };

  return (
    <div className="flex items-center gap-3 border-t border-border bg-background/60 px-3 py-1.5 text-xs">
      <button
        onClick={onPlayPause}
        title={ended ? "Replay from start" : paused ? "Play" : "Pause"}
        className="flex h-6 w-6 shrink-0 items-center justify-center rounded-md border border-border text-muted-foreground transition-colors hover:bg-muted/50"
      >
        {ended ? (
          <RotateCcw className="h-3 w-3" />
        ) : paused ? (
          <Play className="h-3 w-3" />
        ) : (
          <Pause className="h-3 w-3" />
        )}
      </button>

      <span className="shrink-0 tabular-nums text-muted-foreground select-none">
        {formatClock(displayMs)}
      </span>

      <div
        ref={trackRef}
        onPointerDown={onPointerDown}
        onPointerMove={onPointerMove}
        onPointerUp={onPointerUp}
        className="relative h-4 flex-1 cursor-pointer touch-none"
      >
        <div className="absolute inset-x-0 top-1/2 h-1 -translate-y-1/2 rounded-full bg-muted" />
        <div
          className="absolute left-0 top-1/2 h-1 -translate-y-1/2 rounded-full bg-muted-foreground"
          style={{ width: `${frac * 100}%` }}
        />
        <div
          className="absolute top-1/2 h-3 w-3 -translate-x-1/2 -translate-y-1/2 rounded-full bg-muted-foreground shadow ring-2 ring-background"
          style={{ left: `${frac * 100}%` }}
        />
      </div>

      <span className="shrink-0 tabular-nums text-muted-foreground/70 select-none">
        {formatClock(duration)}
      </span>

      <div className="flex shrink-0 overflow-hidden rounded-md border border-border">
        {SPEED_OPTIONS.map((s) => (
          <button
            key={s}
            onClick={() => setPlaybackSpeed(s)}
            className={cn(
              "px-1.5 py-0.5 text-[11px] tabular-nums transition-colors",
              playbackSpeed === s
                ? "bg-accent text-accent-foreground"
                : "text-muted-foreground hover:bg-muted/50",
            )}
          >
            {s}×
          </button>
        ))}
      </div>
    </div>
  );
}
