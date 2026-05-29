import { writeFileSync } from "node:fs";

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
    private controllerUrl: string | undefined,
    private serverUrl: string,
    private outputPath: string,
  ) {}

  private elapsed(): number {
    return Date.now() - this.startTime;
  }

  private addEvent(endpoint: string, payload: string, headers: Record<string, string>): void {
    if (!this.trace[endpoint]) this.trace[endpoint] = [];
    this.trace[endpoint].push({ time: this.elapsed(), payload, headers });
  }

  private buildControllerUrl(path: string, params?: Record<string, string>): string {
    const url = new URL(path, this.serverUrl);
    if (this.controllerUrl) url.searchParams.set("controller", this.controllerUrl);
    if (params) {
      for (const [k, v] of Object.entries(params)) url.searchParams.set(k, v);
    }
    return url.toString();
  }

  private buildSandboxUrl(path: string, sandboxEndpoint: string, params?: Record<string, string>): string {
    const url = new URL(path, this.serverUrl);
    url.searchParams.set("sandboxUrl", sandboxEndpoint);
    if (params) {
      for (const [k, v] of Object.entries(params)) url.searchParams.set(k, v);
    }
    return url.toString();
  }

  private headersToMap(headers: Headers): Record<string, string> {
    const map: Record<string, string> = {};
    headers.forEach((value, key) => {
      map[key] = value;
    });
    return map;
  }

  private pollEndpoint(url: string): void {
    const traceKey = url;
    let lastPayload: string | undefined;

    const poll = async () => {
      try {
        const res = await fetch(url);
        const text = await res.text();
        if (text !== lastPayload) {
          lastPayload = text;
          this.addEvent(traceKey, text, this.headersToMap(res.headers));
        }
      } catch {
        // server not ready or transient error — skip silently
      }
    };

    void poll();
    this.timers.push(setInterval(poll, 1000));
  }

  private streamEndpoint(url: string): void {
    const traceKey = url;
    const ac = new AbortController();
    this.abortControllers.push(ac);

    void (async () => {
      try {
        const res = await fetch(url, { signal: ac.signal });
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

  private trackDirectory(sandboxId: string, sandboxEndpoint: string, dirPath: string, recurse: boolean = false): void {
    const key = `${sandboxId}:${dirPath}`;
    if (this.knownDirs.has(key)) return;
    this.knownDirs.add(key);

    const traceKey = `/api/sandboxes/${sandboxId}/directories?path=${dirPath}`;
    const url = this.buildSandboxUrl(`/api/sandboxes/${sandboxId}/directories`, sandboxEndpoint, { path: dirPath });
    let lastPayload: string | undefined;

    const poll = async () => {
      try {
        const res = await fetch(url);
        const text = await res.text();
        if (text !== lastPayload) {
          lastPayload = text;
          this.addEvent(traceKey, text, this.headersToMap(res.headers));
        }
        if (!recurse) {
          return;
        }
        const { entries } = JSON.parse(text) as { entries: DirEntry[] };
        for (const entry of entries) {
          if (entry.is_dir) {
            this.trackDirectory(sandboxId, sandboxEndpoint, entry.path, recurse);
          } else {
            const ext = entry.path.slice(entry.path.lastIndexOf("."));
            if (TEXT_EXTS.has(ext)) this.trackFile(sandboxId, sandboxEndpoint, entry.path);
          }
        }
      } catch {
        // not ready or parse error — skip
      }
    };

    void poll();
    this.timers.push(setInterval(poll, 1000));
  }

  private trackFile(sandboxId: string, sandboxEndpoint: string, filePath: string): void {
    const key = `${sandboxId}:${filePath}`;
    if (this.knownFiles.has(key)) return;
    this.knownFiles.add(key);

    const traceKey = `/api/sandboxes/${sandboxId}/file?path=${filePath}`;
    const url = this.buildSandboxUrl(`/api/sandboxes/${sandboxId}/file`, sandboxEndpoint, { path: filePath });
    this.pollEndpoint(url);
  }

  start(): void {
    const listUrl = this.buildControllerUrl("/api/sandboxes");

    const fetchOnce = async (): Promise<void> => {
      try {
        const res = await fetch(listUrl);
        const text = await res.text();
        this.addEvent("/api/sandboxes", text, this.headersToMap(res.headers));
        const sandboxes = JSON.parse(text) as { id: string; endpoint: string; exposed_endpoint?: string }[];
        for (const s of sandboxes) this.trackSandbox(s.id, s.endpoint, s.exposed_endpoint);
      } catch {
        // server not up yet — retry in 1 s
        this.timers.push(setTimeout(() => void fetchOnce(), 1000) as unknown as ReturnType<typeof setInterval>);
      }
    };

    void fetchOnce();
  }

  private trackSandbox(id: string, sandboxEndpoint: string, exposedEndpoint?: string): void {
    const configUrl = this.buildSandboxUrl(`/api/sandboxes/${id}/config`, sandboxEndpoint);

    // Fetch config once to discover volume mount paths, then start directory tracking.
    void (async () => {
      const mountPaths: string[] = [];
      try {
        const res = await fetch(configUrl);
        const config = (await res.json()) as { fs?: { mount: string }[] };
        for (const fs of config.fs ?? []) mountPaths.push(fs.mount);
      } catch {
        // config unavailable — fall back to root only
      }
      this.trackDirectory(id, sandboxEndpoint, "/");
      for (const mount of mountPaths) this.trackDirectory(id, sandboxEndpoint, mount, true);
    })();

    this.pollEndpoint(configUrl);
    this.streamEndpoint(this.buildSandboxUrl(`/api/sandboxes/${id}/events`, sandboxEndpoint));
    const sessionId = crypto.randomUUID();
    const params: Record<string, string> = { sessionId };
    if (exposedEndpoint) params.exposedBackend = exposedEndpoint;
    this.streamEndpoint(this.buildSandboxUrl(`/api/sandboxes/${id}/terminal/stream`, sandboxEndpoint, params));
  }

  stop(): void {
    for (const t of this.timers) clearInterval(t);
    for (const ac of this.abortControllers) ac.abort();
    writeFileSync(this.outputPath, JSON.stringify(this.trace, null, 2));
    process.stdout.write(`Recording saved → ${this.outputPath}\n`);
  }
}
