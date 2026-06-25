import { expect, it, vi } from "vitest";
import { Sandbox } from "./sandbox";
import type { ApplyResult, SandboxConfig } from "./schemas";

const GATEWAY = "http://gateway:10000";
const REF = { id: "11111111-1111-1111-1111-111111111111", key: "sb-1" };
const SANDBOX_BASE = `${GATEWAY}/sandbox/${REF.id}`;
const SANDBOX_V1 = `${SANDBOX_BASE}/v1/${REF.key}`;

function makeSandbox(mockFetch: ReturnType<typeof vi.fn>): Sandbox {
  return new Sandbox(REF, {
    gatewayUrl: GATEWAY,
    fetch: mockFetch as unknown as typeof fetch,
  });
}

function jsonResp(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json" },
  });
}

const MIN_CONFIG: SandboxConfig = {
  fs: [{ backend: "local", mount: "/workspace" }],
};

const MIN_APPLY_RESULT: ApplyResult = {
  applied: true,
  config: MIN_CONFIG,
  changes: {},
};

const encoder = new TextEncoder();

function sseBody(events: object[]): ReadableStream<Uint8Array> {
  return new ReadableStream({
    start(controller) {
      for (const evt of events) {
        controller.enqueue(encoder.encode(`data: ${JSON.stringify(evt)}\n\n`));
      }
      controller.close();
    },
  });
}

const STDIO_EVENT = {
  id: 1,
  timestamp: "2024-01-01T00:00:00Z",
  type: "stdio",
  stdout: "hello",
};

// ping

it("ping sends GET /v1/ping", async () => {
  const mockFetch = vi
    .fn()
    .mockResolvedValue(new Response(null, { status: 200 }));
  await makeSandbox(mockFetch).ping();
  const [url] = mockFetch.mock.calls[0] as [string];
  expect(url).toBe(`${SANDBOX_V1}/ping`);
});

it("ping throws SandboxError on non-200", async () => {
  const mockFetch = vi
    .fn()
    .mockResolvedValue(jsonResp({ error: "service unavailable" }, 503));
  await expect(makeSandbox(mockFetch).ping()).rejects.toMatchObject({
    name: "SandboxError",
    status: 503,
    operation: "ping",
  });
});

// getPorts

it("getPorts sends GET /v1/ports and returns the port list", async () => {
  const mockFetch = vi.fn().mockResolvedValue(jsonResp([8080, 9000]));
  const ports = await makeSandbox(mockFetch).getPorts();
  const [url] = mockFetch.mock.calls[0] as [string];
  expect(url).toBe(`${SANDBOX_V1}/ports`);
  expect(ports).toEqual([8080, 9000]);
});

it("getPorts throws SandboxError on non-200", async () => {
  const mockFetch = vi
    .fn()
    .mockResolvedValue(jsonResp({ error: "internal" }, 500));
  await expect(makeSandbox(mockFetch).getPorts()).rejects.toMatchObject({
    name: "SandboxError",
    status: 500,
    operation: "getPorts",
  });
});

// getConfig

it("getConfig sends GET /v1/config and returns parsed SandboxConfig", async () => {
  const mockFetch = vi.fn().mockResolvedValue(jsonResp(MIN_CONFIG));
  const config = await makeSandbox(mockFetch).getConfig();
  const [url] = mockFetch.mock.calls[0] as [string];
  expect(url).toBe(`${SANDBOX_V1}/config`);
  expect(config).toMatchObject(MIN_CONFIG);
});

it("getConfig throws SandboxError on non-200", async () => {
  const mockFetch = vi
    .fn()
    .mockResolvedValue(jsonResp({ error: "not found" }, 404));
  await expect(makeSandbox(mockFetch).getConfig()).rejects.toMatchObject({
    name: "SandboxError",
    status: 404,
    operation: "getConfig",
  });
});

// applyConfig

it("applyConfig sends PUT /v1/config with JSON body", async () => {
  const mockFetch = vi.fn().mockResolvedValue(jsonResp(MIN_APPLY_RESULT));
  await makeSandbox(mockFetch).applyConfig(MIN_CONFIG);
  const [url, init] = mockFetch.mock.calls[0] as [string, RequestInit];
  expect(url).toBe(`${SANDBOX_V1}/config`);
  expect(init.method).toBe("PUT");
  expect((init.headers as Record<string, string>)["content-type"]).toBe(
    "application/json",
  );
  expect(JSON.parse(init.body as string)).toMatchObject(MIN_CONFIG);
});

it("applyConfig returns ApplyResult", async () => {
  const mockFetch = vi.fn().mockResolvedValue(jsonResp(MIN_APPLY_RESULT));
  const result = await makeSandbox(mockFetch).applyConfig(MIN_CONFIG);
  expect(result.applied).toBe(true);
  expect(result.config).toMatchObject(MIN_CONFIG);
});

it("applyConfig throws SandboxError on non-200", async () => {
  const mockFetch = vi
    .fn()
    .mockResolvedValue(jsonResp({ error: "bad config" }, 400));
  await expect(
    makeSandbox(mockFetch).applyConfig(MIN_CONFIG),
  ).rejects.toMatchObject({
    name: "SandboxError",
    status: 400,
    operation: "applyConfig",
  });
});

// readFile

it("readFile sends GET /v1/file?path=<encoded> and returns Uint8Array", async () => {
  const content = new Uint8Array([104, 101, 108, 108, 111]);
  const mockFetch = vi
    .fn()
    .mockResolvedValue(
      new Response(content.buffer as ArrayBuffer, { status: 200 }),
    );
  const result = await makeSandbox(mockFetch).readFile("/workspace/hello.txt");
  const [url] = mockFetch.mock.calls[0] as [URL];
  expect(url.toString()).toBe(
    `${SANDBOX_V1}/file?path=%2Fworkspace%2Fhello.txt`,
  );
  expect(result).toBeInstanceOf(Uint8Array);
  expect(result).toEqual(content);
});

it("readFile throws SandboxError on non-200", async () => {
  const mockFetch = vi
    .fn()
    .mockResolvedValue(jsonResp({ error: "not found" }, 404));
  await expect(
    makeSandbox(mockFetch).readFile("/workspace/missing.txt"),
  ).rejects.toMatchObject({
    name: "SandboxError",
    status: 404,
    operation: "readFile",
  });
});

// writeFile

it("writeFile sends POST /v1/file with multipart form containing destination and file", async () => {
  const mockFetch = vi
    .fn()
    .mockResolvedValue(jsonResp({ path: "/workspace/hello.txt", bytes: 5 }));
  const result = await makeSandbox(mockFetch).writeFile(
    "/workspace",
    "hello.txt",
    "hello",
  );
  const [url, init] = mockFetch.mock.calls[0] as [string, RequestInit];
  expect(url).toBe(`${SANDBOX_V1}/file`);
  expect(init.method).toBe("POST");
  expect(init.body).toBeInstanceOf(FormData);
  const form = init.body as FormData;
  expect(form.get("destination")).toBe("/workspace");
  const file = form.get("file") as File;
  expect(file).toBeInstanceOf(Blob);
  expect(file.name).toBe("hello.txt");
  expect(result).toEqual({ path: "/workspace/hello.txt", bytes: 5 });
});

it("writeFile throws SandboxError on non-200", async () => {
  const mockFetch = vi
    .fn()
    .mockResolvedValue(jsonResp({ error: "destination not mounted" }, 400));
  await expect(
    makeSandbox(mockFetch).writeFile("/workspace", "f.txt", "data"),
  ).rejects.toMatchObject({
    name: "SandboxError",
    status: 400,
    operation: "writeFile",
  });
});

// getEventsStream

it("getEventsStream sends GET /v1/events with accept: text/event-stream header", async () => {
  const ac = new AbortController();
  const mockFetch = vi
    .fn()
    .mockResolvedValue(new Response(sseBody([STDIO_EVENT]), { status: 200 }));
  const gen = makeSandbox(mockFetch).getEventsStream({
    signal: ac.signal,
    maxRetries: 0,
  });
  await gen.next();
  ac.abort();
  const [url, init] = mockFetch.mock.calls[0] as [URL, RequestInit];
  expect(url.toString()).toBe(`${SANDBOX_V1}/events`);
  expect((init.headers as Record<string, string>).accept).toBe(
    "text/event-stream",
  );
});

it("getEventsStream yields parsed SandboxEvents from the SSE stream", async () => {
  const ac = new AbortController();
  const mockFetch = vi
    .fn()
    .mockImplementation(() =>
      Promise.resolve(new Response(sseBody([STDIO_EVENT]), { status: 200 })),
    );
  const events: unknown[] = [];
  for await (const evt of makeSandbox(mockFetch).getEventsStream({
    signal: ac.signal,
  })) {
    events.push(evt);
    ac.abort();
  }
  expect(events).toHaveLength(1);
  expect(events[0]).toMatchObject({ id: 1, type: "stdio", stdout: "hello" });
});

it("getEventsStream stops when abort signal fires", async () => {
  const ac = new AbortController();
  const infiniteBody = new ReadableStream<Uint8Array>({
    start(controller) {
      controller.enqueue(
        encoder.encode(`data: ${JSON.stringify(STDIO_EVENT)}\n\n`),
      );
      // never closes — represents an ongoing event stream
    },
  });
  const mockFetch = vi
    .fn()
    .mockResolvedValue(new Response(infiniteBody, { status: 200 }));
  const events: unknown[] = [];
  for await (const evt of makeSandbox(mockFetch).getEventsStream({
    signal: ac.signal,
  })) {
    events.push(evt);
    ac.abort();
  }
  expect(events).toHaveLength(1);
});

it("getEventsStream reconnects after stream closes and passes lastEventId", async () => {
  const event1 = { ...STDIO_EVENT, id: 5 };
  const event2 = { ...STDIO_EVENT, id: 6 };
  const mockFetch = vi
    .fn()
    .mockResolvedValueOnce(new Response(sseBody([event1]), { status: 200 }))
    .mockResolvedValueOnce(new Response(sseBody([event2]), { status: 200 }));

  const events: unknown[] = [];
  for await (const evt of makeSandbox(mockFetch).getEventsStream({
    maxRetries: 1,
  })) {
    events.push(evt);
  }

  expect(events).toHaveLength(2);
  expect(mockFetch).toHaveBeenCalledTimes(2);
  const [secondUrl] = mockFetch.mock.calls[1] as [URL];
  expect(secondUrl.toString()).toContain("lastEventId=5");
}, 5000);

it("getEventsStream stops after maxRetries when responses are non-200", async () => {
  // maxRetries: 0 is falsy in `opts.maxRetries || 3`, so use 1 → 2 total attempts
  const mockFetch = vi
    .fn()
    .mockImplementation(() =>
      Promise.resolve(jsonResp({ error: "internal server error" }, 500)),
    );
  const events: unknown[] = [];
  for await (const evt of makeSandbox(mockFetch).getEventsStream({
    maxRetries: 1,
  })) {
    events.push(evt);
  }
  expect(events).toHaveLength(0);
  expect(mockFetch).toHaveBeenCalledTimes(2);
}, 5000);
