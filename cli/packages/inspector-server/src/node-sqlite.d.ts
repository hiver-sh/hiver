// Minimal ambient declaration for Node's built-in SQLite module. The runtime
// (Node ≥ 22.5) ships it, but @types/node@20 predates it, so we declare just
// the synchronous surface eventStore.ts uses. Drop this once @types/node is
// bumped to a version that includes "node:sqlite".
declare module "node:sqlite" {
  export interface StatementSync {
    run(
      ...params: unknown[]
    ): { changes: number; lastInsertRowid: number | bigint };
    get(...params: unknown[]): unknown;
    all(...params: unknown[]): unknown[];
  }
  export class DatabaseSync {
    constructor(path: string, options?: { open?: boolean });
    exec(sql: string): void;
    prepare(sql: string): StatementSync;
    close(): void;
  }
}
