import { expect, it, vi } from "vitest";
import {
  getOrCreateSandbox,
  shutdown,
  DEFAULT_CONTROLLER_URL,
} from "./controller";
import { Sandbox, SandboxError } from "./sandbox";
import type { SandboxConfig } from "./schemas";

const SANDBOX_REF = { id: "test-sandbox", endpoint: "http://sandbox:8080" };
const BASE_CONFIG: SandboxConfig = {
  fs: [{ backend: "local", mount: "/workspace" }],
};

function jsonResp(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json" },
  });
}

function mockSandbox(mockFetch: ReturnType<typeof vi.fn>): Sandbox {
  return new Sandbox(SANDBOX_REF, {
    controllerUrl: DEFAULT_CONTROLLER_URL,
    fetch: mockFetch as unknown as typeof fetch,
  });
}

// getOrCreateSandbox

it("getOrCreateSandbox sends PUT /v1/sandboxes/{id} with JSON body", async () => {
  const mockFetch = vi.fn().mockResolvedValue(jsonResp(SANDBOX_REF));
  await getOrCreateSandbox("test-sandbox", BASE_CONFIG, {
    fetch: mockFetch as unknown as typeof fetch,
    readinessTimeoutMs: 0,
  });

  expect(mockFetch).toHaveBeenCalledOnce();
  const [url, init] = mockFetch.mock.calls[0] as [string, RequestInit];
  expect(url).toBe(`${DEFAULT_CONTROLLER_URL}/v1/sandboxes/test-sandbox`);
  expect(init.method).toBe("PUT");
  expect((init.headers as Record<string, string>)["content-type"]).toBe(
    "application/json",
  );
  expect(JSON.parse(init.body as string)).toMatchObject(BASE_CONFIG);
});

it("getOrCreateSandbox returns Sandbox with correct id and endpoint on 200", async () => {
  const mockFetch = vi.fn().mockResolvedValue(jsonResp(SANDBOX_REF));
  const sandbox = await getOrCreateSandbox("test-sandbox", BASE_CONFIG, {
    fetch: mockFetch as unknown as typeof fetch,
    readinessTimeoutMs: 0,
  });
  expect(sandbox).toBeInstanceOf(Sandbox);
  expect(sandbox.id).toBe("test-sandbox");
  expect(sandbox.apiServerUrl).toBe("http://sandbox:8080");
});

it("getOrCreateSandbox returns Sandbox on 201 (created)", async () => {
  const mockFetch = vi.fn().mockResolvedValue(jsonResp(SANDBOX_REF, 201));
  const sandbox = await getOrCreateSandbox("test-sandbox", BASE_CONFIG, {
    fetch: mockFetch as unknown as typeof fetch,
    readinessTimeoutMs: 0,
  });
  expect(sandbox).toBeInstanceOf(Sandbox);
});

it("getOrCreateSandbox uses custom controllerUrl", async () => {
  const mockFetch = vi.fn().mockResolvedValue(jsonResp(SANDBOX_REF));
  await getOrCreateSandbox("test-sandbox", BASE_CONFIG, {
    fetch: mockFetch as unknown as typeof fetch,
    controllerUrl: "http://custom-controller:1234",
    readinessTimeoutMs: 0,
  });
  const [url] = mockFetch.mock.calls[0] as [string];
  expect(url).toBe("http://custom-controller:1234/v1/sandboxes/test-sandbox");
});

it("getOrCreateSandbox throws Error for id that does not match pattern", async () => {
  await expect(
    getOrCreateSandbox("invalid id!", BASE_CONFIG, { readinessTimeoutMs: 0 }),
  ).rejects.toThrow(/must match/);
});

it("getOrCreateSandbox throws SandboxError on 4xx response", async () => {
  const mockFetch = vi
    .fn()
    .mockResolvedValue(jsonResp({ error: "conflict" }, 409));
  await expect(
    getOrCreateSandbox("test-sandbox", BASE_CONFIG, {
      fetch: mockFetch as unknown as typeof fetch,
      readinessTimeoutMs: 0,
    }),
  ).rejects.toMatchObject({
    name: "SandboxError",
    status: 409,
    operation: "getOrCreateSandbox",
  });
});

it("getOrCreateSandbox throws SandboxError with 'connection refused' on ECONNREFUSED", async () => {
  const err = Object.assign(new Error("connect failed"), {
    cause: { code: "ECONNREFUSED" },
  });
  const mockFetch = vi.fn().mockRejectedValue(err);
  await expect(
    getOrCreateSandbox("test-sandbox", BASE_CONFIG, {
      fetch: mockFetch as unknown as typeof fetch,
      readinessTimeoutMs: 0,
    }),
  ).rejects.toMatchObject({
    name: "SandboxError",
    status: 0,
    message: expect.stringContaining("connection refused"),
  });
});

it("getOrCreateSandbox polls /v1/ping before resolving when readinessTimeoutMs > 0", async () => {
  const mockFetch = vi
    .fn()
    .mockResolvedValueOnce(jsonResp(SANDBOX_REF))
    .mockResolvedValue(new Response(null, { status: 200 }));

  const sandbox = await getOrCreateSandbox("test-sandbox", BASE_CONFIG, {
    fetch: mockFetch as unknown as typeof fetch,
    readinessTimeoutMs: 2000,
  });

  expect(sandbox).toBeInstanceOf(Sandbox);
  expect(mockFetch.mock.calls.length).toBeGreaterThanOrEqual(2);
  const pingCall = mockFetch.mock.calls.find((args) =>
    (args[0] as string).includes("/v1/ping"),
  );
  expect(pingCall).toBeDefined();
});

// shutdown

it("shutdown sends POST /v1/shutdown/{id}", async () => {
  const mockFetch = vi
    .fn()
    .mockResolvedValue(new Response(null, { status: 204 }));
  await shutdown(mockSandbox(mockFetch));
  const [url, init] = mockFetch.mock.calls[0] as [string, RequestInit];
  expect(url).toBe(`${DEFAULT_CONTROLLER_URL}/v1/shutdown/test-sandbox`);
  expect(init.method).toBe("POST");
});

it("shutdown resolves on 204", async () => {
  const mockFetch = vi
    .fn()
    .mockResolvedValue(new Response(null, { status: 204 }));
  await expect(shutdown(mockSandbox(mockFetch))).resolves.toBeUndefined();
});

it("shutdown throws SandboxError on non-204 response", async () => {
  const mockFetch = vi
    .fn()
    .mockResolvedValue(jsonResp({ error: "not found" }, 404));
  await expect(shutdown(mockSandbox(mockFetch))).rejects.toMatchObject({
    name: "SandboxError",
    status: 404,
    operation: "shutdown",
  });
});

it("shutdown throws SandboxError with 'connection refused' on ECONNREFUSED", async () => {
  const err = Object.assign(new Error("connect failed"), {
    cause: { code: "ECONNREFUSED" },
  });
  const mockFetch = vi.fn().mockRejectedValue(err);
  await expect(shutdown(mockSandbox(mockFetch))).rejects.toMatchObject({
    name: "SandboxError",
    status: 0,
    message: expect.stringContaining("connection refused"),
  });
});
