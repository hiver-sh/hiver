import { DatabaseSync } from "node:sqlite";
import { mkdirSync } from "node:fs";
import { homedir } from "node:os";
import { join } from "node:path";
import type { SandboxEvent } from "@hiver.sh/client";

// A relayed feed event carries the identity of the sandbox it came from. When
// that's a different (nested) sandbox than the stream owner, we keep its own
// identity and broker event id; for the owner's own events those fields are
// absent.
export type StoredEvent = SandboxEvent & {
  sandbox_id?: string;
  sandbox_key?: string;
};

const HIVER_DIR = join(homedir(), ".hiver");
const DB_PATH = join(HIVER_DIR, "events.db");

// Bump whenever the events schema changes: an older on-disk DB is wiped and
// recreated. Safe across processes — a DB already at this version is left alone,
// so `hiver events` and the inspector never clobber each other's live data.
const SCHEMA_VERSION = 4;

// How long events are kept before pruneOldEvents removes them. Configurable via
// HIVER_EVENTS_TTL_HOURS (default 24h); set to 0 (or a negative/invalid value)
// to keep events forever.
const ttlHours = Number(process.env.HIVER_EVENTS_TTL_HOURS ?? 24);
const DEFAULT_TTL_MS =
  Number.isFinite(ttlHours) && ttlHours > 0 ? ttlHours * 60 * 60 * 1000 : 0;

let db: DatabaseSync | null = null;

function getDb(): DatabaseSync {
  if (db) return db;
  mkdirSync(HIVER_DIR, { recursive: true });
  const d = new DatabaseSync(DB_PATH);
  // WAL lets the inspector server and `hiver events` write the same file from
  // separate processes without blocking each other's reads.
  d.exec("PRAGMA journal_mode = WAL");
  // Drop tables left over from an older schema, then stamp the current version.
  const { user_version: version } = d.prepare("PRAGMA user_version").get() as {
    user_version: number;
  };
  if (version < SCHEMA_VERSION) d.exec("DROP TABLE IF EXISTS events");
  d.exec(`PRAGMA user_version = ${SCHEMA_VERSION}`);
  // Each row is one event in an owner's inspector timeline:
  //   owner_id / owner_key / owner_event_id
  //     — the sandbox whose /stream (or `hiver events`) captured this event, and
  //       a monotonic per-owner sequence we assign as events arrive. This triple
  //       is the primary key and the single ordered cursor over the whole feed
  //       (the primary sandbox plus every nested sandbox relayed into it).
  //   nested_id / nested_key / nested_event_id
  //     — NULL when the owner is itself the origin (the primary's own events);
  //       otherwise the DIFFERENT (nested) sandbox that originated the event,
  //       plus that sandbox's own broker event id, which we resume it from.
  d.exec(`
    CREATE TABLE IF NOT EXISTS events (
      owner_id         TEXT    NOT NULL,
      owner_key        TEXT    NOT NULL,
      owner_event_id   INTEGER NOT NULL,
      nested_id        TEXT,
      nested_key       TEXT,
      nested_event_id  INTEGER,
      timestamp        INTEGER,
      data             TEXT    NOT NULL,
      PRIMARY KEY (owner_id, owner_key, owner_event_id)
    )
  `);
  d.exec(
    "CREATE INDEX IF NOT EXISTS idx_events_owner ON events(owner_id, owner_key)",
  );
  db = d;
  // Sweep events past the TTL whenever the DB is opened (server start, each
  // `hiver events` run) so stale rows don't accumulate.
  pruneOldEvents();
  return d;
}

// Does this event come from a different sandbox than its owner? Relayed nested
// events carry a sandbox_id/sandbox_key that differs from the owner; the owner's
// own events either omit them or repeat the owner's identity.
function nestedOrigin(
  ownerId: string,
  ownerKey: string,
  event: StoredEvent,
): { id: string; key: string } | null {
  const { sandbox_id: id, sandbox_key: key } = event;
  if (!id || !key) return null;
  if (id === ownerId && key === ownerKey) return null;
  return { id, key };
}

// Persist one event under an owning sandbox. owner_event_id is the next per-owner
// sequence value — the event's position in this owner's feed. INSERT OR IGNORE
// guards the rare case where two writers pick the same next value. The nested_*
// columns are set only when a different sandbox originated the event.
export function appendEvent(
  ownerId: string,
  ownerKey: string,
  event: StoredEvent,
): void {
  const d = getDb();
  const { next } = d
    .prepare(
      "SELECT COALESCE(MAX(owner_event_id), 0) + 1 AS next FROM events WHERE owner_id = ? AND owner_key = ?",
    )
    .get(ownerId, ownerKey) as { next: number };
  const nested = nestedOrigin(ownerId, ownerKey, event);
  // Store the timestamp as epoch ms so TTL pruning is a simple numeric compare.
  const ts = event.timestamp ? Date.parse(event.timestamp) : NaN;
  d.prepare(
    "INSERT OR IGNORE INTO events (owner_id, owner_key, owner_event_id, nested_id, nested_key, nested_event_id, timestamp, data) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
  ).run(
    ownerId,
    ownerKey,
    next,
    nested?.id ?? null,
    nested?.key ?? null,
    nested ? event.id : null,
    Number.isNaN(ts) ? null : ts,
    JSON.stringify(event),
  );
}

// Delete events older than `maxAgeMs` (default: the TTL from
// HIVER_EVENTS_TTL_HOURS, 24h). No-op when the TTL is disabled (<= 0); events
// with no timestamp are never aged out.
export function pruneOldEvents(maxAgeMs: number = DEFAULT_TTL_MS): void {
  if (maxAgeMs <= 0) return;
  getDb()
    .prepare("DELETE FROM events WHERE timestamp IS NOT NULL AND timestamp < ?")
    .run(Date.now() - maxAgeMs);
}

// All events captured under one owner (the primary's own events plus any nested
// sandbox events relayed into its timeline), in feed order (owner_event_id).
export function loadEvents(ownerId: string, ownerKey: string): StoredEvent[] {
  const rows = getDb()
    .prepare(
      "SELECT data FROM events WHERE owner_id = ? AND owner_key = ? ORDER BY owner_event_id ASC",
    )
    .all(ownerId, ownerKey) as { data: string }[];
  return rows.map((r) => JSON.parse(r.data) as StoredEvent);
}

// Highest broker event id stored for a primary's OWN feed (rows where the owner
// is the origin, so nested_id IS NULL). Its broker id lives in the event payload
// rather than a column, since it isn't a nested sandbox. Used to resume the
// primary upstream stream from exactly after its last persisted event.
export function lastOwnEventId(
  ownerId: string,
  ownerKey: string,
): number | undefined {
  const row = getDb()
    .prepare(
      "SELECT MAX(CAST(json_extract(data, '$.id') AS INTEGER)) AS maxId FROM events WHERE owner_id = ? AND owner_key = ? AND nested_id IS NULL",
    )
    .get(ownerId, ownerKey) as { maxId: number | null } | undefined;
  return row?.maxId ?? undefined;
}

// Highest broker event id stored for a nested sandbox, or undefined when none.
// Used to resume that nested sandbox's stream where it left off.
export function lastNestedEventId(
  nestedId: string,
  nestedKey: string,
): number | undefined {
  const row = getDb()
    .prepare(
      "SELECT MAX(nested_event_id) AS maxId FROM events WHERE nested_id = ? AND nested_key = ?",
    )
    .get(nestedId, nestedKey) as { maxId: number | null } | undefined;
  return row?.maxId ?? undefined;
}

// Distinct nested sandboxes recorded under an owner. On reconnect the inspector
// resumes each of these from its lastNestedEventId, even without seeing a fresh
// linking egress.
export function linkedSandboxes(
  ownerId: string,
  ownerKey: string,
): { id: string; key: string }[] {
  const rows = getDb()
    .prepare(
      "SELECT DISTINCT nested_id, nested_key FROM events WHERE owner_id = ? AND owner_key = ? AND nested_id IS NOT NULL",
    )
    .all(ownerId, ownerKey) as { nested_id: string; nested_key: string }[];
  return rows.map((r) => ({ id: r.nested_id, key: r.nested_key }));
}

// Drop everything captured under one owner (its own events and its nested
// sandboxes'), on clear / shutdown / teardown.
export function clearEvents(ownerId: string, ownerKey: string): void {
  getDb()
    .prepare("DELETE FROM events WHERE owner_id = ? AND owner_key = ?")
    .run(ownerId, ownerKey);
}
