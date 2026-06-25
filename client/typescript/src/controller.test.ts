import { expect, it, vi } from "vitest";
import {
  getOrCreateSandbox,
  DEFAULT_GATEWAY_URL,
  DEFAULT_IMAGE_NAME,
} from "./controller";
import { Sandbox } from "./sandbox";
import type { SandboxConfig } from "./schemas";

const SANDBOX_ID = "11111111-1111-1111-1111-111111111111";
const SANDBOX_REF = { id: SANDBOX_ID, key: "test-sandbox" };
const BASE_CONFIG: SandboxConfig = {
  fs: [{ backend: "local", mount: "/workspace" }],
};

function jsonResp(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json" },
  });
}

// getOrCreateSandbox

it("getOrCreateSandbox sends POST /v1/sandboxes/{id} with JSON body and x-hiver-image", async () => {
  const mockFetch = vi.fn().mockResolvedValue(jsonResp(SANDBOX_REF));
  await getOrCreateSandbox("test-sandbox", BASE_CONFIG, {
    fetch: mockFetch as unknown as typeof fetch,
    timeoutMs: 0,
  });

  expect(mockFetch).toHaveBeenCalledOnce();
  const [url, init] = mockFetch.mock.calls[0] as [string, RequestInit];
  expect(url).toBe(`${DEFAULT_GATEWAY_URL}/v1/sandboxes/test-sandbox`);
  expect(init.method).toBe("POST");
  const headers = init.headers as Record<string, string>;
  expect(headers["content-type"]).toBe("application/json");
  // No image in config → the default logical image name routes the create.
  expect(headers["x-hiver-image"]).toBe(DEFAULT_IMAGE_NAME);
  expect(JSON.parse(init.body as string)).toMatchObject(BASE_CONFIG);
});

it("getOrCreateSandbox sends config.image as the x-hiver-image header", async () => {
  const mockFetch = vi.fn().mockResolvedValue(jsonResp(SANDBOX_REF));
  await getOrCreateSandbox(
    "test-sandbox",
    { ...BASE_CONFIG, image: "browser" },
    { fetch: mockFetch as unknown as typeof fetch, timeoutMs: 0 },
  );
  const [, init] = mockFetch.mock.calls[0] as [string, RequestInit];
  expect((init.headers as Record<string, string>)["x-hiver-image"]).toBe(
    "browser",
  );
});


it("getOrCreateSandbox returns Sandbox with correct id, key and apiServerUrl on 200", async () => {
  const mockFetch = vi.fn().mockResolvedValue(jsonResp(SANDBOX_REF));
  const sandbox = await getOrCreateSandbox("test-sandbox", BASE_CONFIG, {
    fetch: mockFetch as unknown as typeof fetch,
    timeoutMs: 0,
  });
  expect(sandbox).toBeInstanceOf(Sandbox);
  expect(sandbox.id).toBe(SANDBOX_ID);
  expect(sandbox.key).toBe("test-sandbox");
  expect(sandbox.apiServerUrl).toBe(
    `${DEFAULT_GATEWAY_URL}/sandbox/${SANDBOX_ID}`,
  );
});

it("getOrCreateSandbox returns Sandbox on 201 (created)", async () => {
  const mockFetch = vi.fn().mockResolvedValue(jsonResp(SANDBOX_REF, 201));
  const sandbox = await getOrCreateSandbox("test-sandbox", BASE_CONFIG, {
    fetch: mockFetch as unknown as typeof fetch,
    timeoutMs: 0,
  });
  expect(sandbox).toBeInstanceOf(Sandbox);
});

it("getOrCreateSandbox uses custom gatewayUrl", async () => {
  const mockFetch = vi.fn().mockResolvedValue(jsonResp(SANDBOX_REF));
  await getOrCreateSandbox("test-sandbox", BASE_CONFIG, {
    fetch: mockFetch as unknown as typeof fetch,
    gatewayUrl: "http://custom-gateway:1234",
    timeoutMs: 0,
  });
  const [url] = mockFetch.mock.calls[0] as [string];
  expect(url).toBe("http://custom-gateway:1234/v1/sandboxes/test-sandbox");
});

it("getOrCreateSandbox throws Error for id that does not match pattern", async () => {
  await expect(
    getOrCreateSandbox("invalid id!", BASE_CONFIG, { timeoutMs: 0 }),
  ).rejects.toThrow(/must match/);
});

it("getOrCreateSandbox throws SandboxError on 4xx response", async () => {
  const mockFetch = vi
    .fn()
    .mockResolvedValue(jsonResp({ error: "conflict" }, 409));
  await expect(
    getOrCreateSandbox("test-sandbox", BASE_CONFIG, {
      fetch: mockFetch as unknown as typeof fetch,
      timeoutMs: 0,
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
      timeoutMs: 0,
    }),
  ).rejects.toMatchObject({
    name: "SandboxError",
    status: 0,
    message: expect.stringContaining("connection refused"),
  });
});
