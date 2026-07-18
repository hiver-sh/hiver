import { z } from "zod";

export const SandboxRef = z.object({
  // Server-assigned unique identifier (the container/pod ID). Used in API
  // URLs for routing to the correct pod. In pack mode multiple sandboxes
  // share the same id. The IndexedDB partition key is `${id}:${key}` so
  // that packed sandboxes are isolated and key-reuse across lifetimes
  // (different id) never collides.
  id: z.string(),
  // Caller-chosen key the sandbox was provisioned under. Unique per sandbox
  // even in pack mode. Used for routing, selection, and display.
  key: z.string(),
  status: z.enum(["start", "stop", "die"]).optional(),
});
export type SandboxRef = z.infer<typeof SandboxRef>;

// Mirror of the server-side SandboxEvent discriminated union.
// sandbox_key is injected by the inspector server to identify which sandbox
// emitted the event; linked sandboxes use the x-hiver-sandbox-key header value.
type SandboxEventVariant =
  | {
      id: number;
      timestamp: string;
      type: "config.apply";
      success: boolean;
      changes: unknown;
      errorMessage?: string;
    }
  | {
      id: number;
      timestamp: string;
      type: "egress.request";
      access: "allowed" | "denied";
      host: string;
      method: string;
      path: string;
      query?: string;
      headers?: Record<string, string>;
      body?: string;
    }
  | {
      id: number;
      timestamp: string;
      type: "egress.response";
      request_id: number;
      status: number;
      duration_ms: number;
      headers?: Record<string, string>;
      body?: string;
    }
  | {
      id: number;
      timestamp: string;
      type: "egress.chunk";
      request_id: number;
      body: string;
      label?: string;
    }
  | {
      id: number;
      timestamp: string;
      type: "fs.request";
      access: "allowed" | "denied";
      mount: string;
      path: string;
      operation: "read" | "write" | "delete";
    }
  | {
      id: number;
      timestamp: string;
      type: "fs.response";
      backend: string;
      request_id: number;
      duration_ms: number;
      error?: string;
    }
  | {
      id: number;
      timestamp: string;
      type: "stdio";
      stdout?: string;
      stderr?: string;
    }
  | {
      id: number;
      timestamp: string;
      type: "resource.usage";
      cpu_percent: number;
      memory_bytes: number;
    }
  | {
      id: number;
      timestamp: string;
      type: "exec.request";
      cwd: string;
      command: string;
    }
  | { id: number; timestamp: string; type: "exec.response"; request_id: number }
  | {
      id: number;
      timestamp: string;
      type: "ingress.request";
      port: string;
      method: string;
      path: string;
      query?: string;
      headers?: Record<string, string>;
      body?: string;
    }
  | {
      id: number;
      timestamp: string;
      type: "ingress.response";
      request_id: number;
      status: number;
      duration_ms: number;
      headers?: Record<string, string>;
      body?: string;
    }
  | {
      id: number;
      timestamp: string;
      type: "system.start" | "system.config-changed" | "system.shutdown";
      // Present on system.config-changed: the config in effect after the change.
      config?: unknown;
    };

export type SandboxEvent = SandboxEventVariant & {
  // Injected by the inspector server to identify which sandbox emitted the
  // event. For the primary sandbox these are its path id/key; for linked
  // sandboxes they come from the x-hiver-sandbox-id / x-hiver-sandbox-key
  // header values. Used to route allow/deny policy edits back to the right
  // sandbox.
  sandbox_id?: string;
  sandbox_key?: string;
};

// Identifies a specific sandbox for routing API calls (e.g. policy edits) to it.
export interface SandboxTarget {
  id: string;
  key: string;
}

export const DEFAULT_INSPECTOR_SERVER = import.meta.env.PROD
  ? window.location.origin
  : "http://localhost:3001";
export const DEFAULT_GATEWAY_URL = "http://localhost:10000";
