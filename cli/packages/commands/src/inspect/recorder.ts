import { writeFileSync } from "node:fs";
import { extname, join } from "node:path";

interface RecordedEvent {
  time: number;
  payload: string;
  headers: Record<string, string>;
}

type Trace = Record<string, RecordedEvent[]>;

const TEXT_EXTS = new Set([".md", ".py", ".json", ".txt", ".js", ".xml"]);

interface DirEntry {
  path: string;
  is_dir: boolean;
}

export class EventRecorder {
  private trace: Trace = {};
  private startTime = Date.now();
  private timers: ReturnType<typeof setInterval>[] = [];
  private abortControllers: AbortController[] = [];
  private knownDirs = new Set<string>();
  private knownFiles = new Set<string>();

  constructor(
    private gatewayUrl: string | undefined,
    private serverUrl: string,
    private outputPath: string,
  ) {}

  private elapsed(): number {
    return Date.now() - this.startTime;
  }

  private addEvent(
    endpoint: string,
    payload: string,
    headers: Record<string, string>,
  ): void {
    if (!this.trace[endpoint]) this.trace[endpoint] = [];
    this.trace[endpoint].push({ time: this.elapsed(), payload, headers });
  }

  private buildUrl(path: string, params?: Record<string, string>): string {
    const url = new URL(path, this.serverUrl);
    if (params) {
      for (const [k, v] of Object.entries(params)) url.searchParams.set(k, v);
    }
    return url.toString();
  }

  private gatewayHeaders(): Record<string, string> {
    return this.gatewayUrl ? { "x-gateway-url": this.gatewayUrl } : {};
  }

  private headersToMap(headers: Headers): Record<string, string> {
    const map: Record<string, string> = {};
    headers.forEach((value, key) => {
      map[key] = value;
    });
    return map;
  }

  private pollEndpoint(url: string): void {
    const traceKey = new URL(url).pathname + (new URL(url).search || "");
    let lastPayload: string | undefined;

    const poll = async () => {
      try {
        const res = await fetch(url, { headers: this.gatewayHeaders() });
        const text = await res.text();
        if (text !== lastPayload) {
          lastPayload = text;
          this.addEvent(traceKey, text, this.headersToMap(res.headers));
        }
      } catch (e) {
        console.error(e);
      }
    };

    void poll();
    this.timers.push(setInterval(poll, 1000));
  }

  private streamEndpoint(url: string): void {
    const traceKey = new URL(url).pathname + (new URL(url).search || "");
    const ac = new AbortController();
    this.abortControllers.push(ac);

    void (async () => {
      try {
        const res = await fetch(url, {
          signal: ac.signal,
          headers: this.gatewayHeaders(),
        });
        const responseHeaders = this.headersToMap(res.headers);
        if (!res.body) return;

        const reader = res.body.getReader();
        const decoder = new TextDecoder();
        let buffer = "";

        while (true) {
          const { done, value } = await reader.read();
          if (done) break;
          buffer += decoder.decode(value, { stream: true });

          // SSE events are delimited by double newline
          const parts = buffer.split("\n\n");
          buffer = parts.pop() ?? "";

          for (const part of parts) {
            const trimmed = part.trim();
            if (trimmed) this.addEvent(traceKey, trimmed, responseHeaders);
          }
        }
      } catch (e) {
        console.error(e);
        // aborted or connection closed
      }
    })();
  }

  private trackDirectory(sandboxKey: string, dirPath: string): void {
    const key = `${sandboxKey}:${dirPath}`;
    if (this.knownDirs.has(key)) return;
    this.knownDirs.add(key);

    const traceKey = `/api/sandboxes/${sandboxKey}/directories?path=${dirPath}`;
    const url = this.buildUrl(`/api/sandboxes/${sandboxKey}/directories`, {
      path: dirPath,
    });
    let lastPayload: string | undefined;

    const poll = async () => {
      try {
        const res = await fetch(url, { headers: this.gatewayHeaders() });
        const text = await res.text();
        if (text !== lastPayload) {
          lastPayload = text;
          this.addEvent(traceKey, text, this.headersToMap(res.headers));
          const { entries } = JSON.parse(text) as { entries: DirEntry[] };
          for (const entry of entries) {
            const fullPath = entry.path;
            if (!entry.is_dir && TEXT_EXTS.has(extname(fullPath))) {
              this.trackFile(sandboxKey, fullPath);
            }
          }
        }
      } catch (e) {
        console.error(e);
      }
    };

    void poll();
    this.timers.push(setInterval(poll, 1000));
  }

  private trackFile(sandboxKey: string, filePath: string): void {
    const key = `${sandboxKey}:${filePath}`;
    if (this.knownFiles.has(key)) return;
    this.knownFiles.add(key);

    const url = this.buildUrl(`/api/sandboxes/${sandboxKey}/file`, {
      path: filePath,
    });
    this.pollEndpoint(url);
  }

  start(): void {
    const listUrl = this.buildUrl("/api/sandboxes");

    const fetchOnce = async (): Promise<void> => {
      try {
        const res = await fetch(listUrl, { headers: this.gatewayHeaders() });
        const text = await res.text();
        this.addEvent("/api/sandboxes", text, this.headersToMap(res.headers));
        const sandboxes = JSON.parse(text) as {
          id: string;
          key: string;
        }[];
        for (const s of sandboxes) this.trackSandbox(s.key);
      } catch (e) {
        console.error(e);
        this.timers.push(
          setTimeout(() => void fetchOnce(), 1000) as unknown as ReturnType<
            typeof setInterval
          >,
        );
      }
    };

    void fetchOnce();
  }

  private trackSandbox(sandboxKey: string): void {
    const configUrl = this.buildUrl(`/api/sandboxes/${sandboxKey}/config`);

    // Fetch config once to discover volume mount paths, then start directory tracking.
    void (async () => {
      const mountPaths: string[] = [];
      try {
        const res = await fetch(configUrl, { headers: this.gatewayHeaders() });
        const config = (await res.json()) as { fs?: { mount: string }[] };
        for (const fs of config.fs ?? []) mountPaths.push(fs.mount);
      } catch (e) {
        console.error(e);
      }
      this.trackDirectory(sandboxKey, "/");
      for (const mount of mountPaths) this.trackDirectory(sandboxKey, mount);
    })();

    this.pollEndpoint(configUrl);
    this.pollEndpoint(this.buildUrl(`/api/sandboxes/${sandboxKey}/ports`));
    this.streamEndpoint(this.buildUrl(`/api/sandboxes/${sandboxKey}/events`));
    this.streamEndpoint(
      this.buildUrl(`/api/sandboxes/${sandboxKey}/terminal/stream`),
    );
  }

  stop(): void {
    for (const t of this.timers) clearInterval(t);
    for (const ac of this.abortControllers) ac.abort();
    writeFileSync(this.outputPath, JSON.stringify(this.trace, null, 2));
    process.stdout.write(`Recording saved → ${this.outputPath}\n`);
  }
}
