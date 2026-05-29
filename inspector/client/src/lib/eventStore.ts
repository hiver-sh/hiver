import type { SandboxEvent } from "@/types";

const DB_NAME = "inspector-events";
const DB_VERSION = 1;
const STORE = "events";

let dbPromise: Promise<IDBDatabase> | null = null;

function getDb(): Promise<IDBDatabase> {
  if (!dbPromise) {
    dbPromise = new Promise((resolve, reject) => {
      const req = indexedDB.open(DB_NAME, DB_VERSION);
      req.onupgradeneeded = () => {
        const db = req.result;
        if (!db.objectStoreNames.contains(STORE)) {
          const store = db.createObjectStore(STORE, { keyPath: ["sandboxId", "id"] });
          store.createIndex("bySandbox", "sandboxId");
        }
      };
      req.onsuccess = () => resolve(req.result);
      req.onerror = () => reject(req.error);
    });
  }
  return dbPromise;
}

// Returns events sorted by id ascending (IDB orders by compound primary key).
export async function loadEvents(sandboxId: string): Promise<SandboxEvent[]> {
  const db = await getDb();
  return new Promise((resolve, reject) => {
    const tx = db.transaction(STORE, "readonly");
    const req = tx.objectStore(STORE).index("bySandbox").getAll(IDBKeyRange.only(sandboxId));
    req.onsuccess = () => {
      resolve(
        (req.result as Array<Record<string, unknown>>).map((row) => {
          const { sandboxId: _s, ...event } = row;
          return event as unknown as SandboxEvent;
        }),
      );
    };
    req.onerror = () => reject(req.error);
  });
}

export async function appendEvent(sandboxId: string, event: SandboxEvent): Promise<void> {
  const db = await getDb();
  return new Promise((resolve, reject) => {
    const tx = db.transaction(STORE, "readwrite");
    tx.objectStore(STORE).put({ ...event, sandboxId });
    tx.oncomplete = () => resolve();
    tx.onerror = () => reject(tx.error);
  });
}

export async function clearEvents(sandboxId: string): Promise<void> {
  const db = await getDb();
  return new Promise((resolve, reject) => {
    const tx = db.transaction(STORE, "readwrite");
    const store = tx.objectStore(STORE);
    const req = store.index("bySandbox").openKeyCursor(IDBKeyRange.only(sandboxId));
    req.onsuccess = () => {
      const cursor = req.result;
      if (cursor) {
        store.delete(cursor.primaryKey);
        cursor.continue();
      }
    };
    tx.oncomplete = () => resolve();
    tx.onerror = () => reject(tx.error);
  });
}

async function listStoredSandboxIds(): Promise<string[]> {
  const db = await getDb();
  return new Promise((resolve, reject) => {
    const tx = db.transaction(STORE, "readonly");
    const req = tx.objectStore(STORE).index("bySandbox").openKeyCursor(null, "nextunique");
    const ids: string[] = [];
    req.onsuccess = () => {
      const cursor = req.result;
      if (cursor) {
        ids.push(cursor.key as string);
        cursor.continue();
      }
    };
    tx.oncomplete = () => resolve(ids);
    tx.onerror = () => reject(tx.error);
  });
}

const PURGE_GRACE_MS = 30_000;
const absentSince = new Map<string, number>();

export async function purgeOrphanEvents(activeSandboxIds: string[]): Promise<void> {
  const stored = await listStoredSandboxIds();
  const active = new Set(activeSandboxIds);
  const now = Date.now();

  for (const id of stored) {
    if (active.has(id)) {
      absentSince.delete(id);
    } else if (!absentSince.has(id)) {
      absentSince.set(id, now);
    }
  }
  for (const id of absentSince.keys()) {
    if (!stored.includes(id)) absentSince.delete(id);
  }

  const toEvict = stored.filter(
    (id) => !active.has(id) && now - (absentSince.get(id) ?? now) > PURGE_GRACE_MS,
  );
  await Promise.all(toEvict.map((id) => { absentSince.delete(id); return clearEvents(id); }));
}
