import { z } from "zod";

export const SandboxRef = z.object({
  // Server-assigned unique identifier (uuid). Used only to partition
  // persisted events in IndexedDB, where a reused key must not collide.
  id: z.string(),
  // Caller-chosen key the sandbox was provisioned under. Used for routing,
  // selection, and display everywhere in the UI.
  key: z.string(),
  status: z.enum(["start", "stop", "die"]).optional(),
});
export type SandboxRef = z.infer<typeof SandboxRef>;

// Mirror of the server-side SandboxEvent discriminated union
export type SandboxEvent =
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
      operation: "read" | "write";
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
    };

export const DEFAULT_INSPECTOR_SERVER = import.meta.env.PROD
  ? window.location.origin
  : "http://localhost:3001";
export const DEFAULT_GATEWAY_URL = "http://localhost:10000";
