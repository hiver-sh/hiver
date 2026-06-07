import { createServer } from "node:net";

/** Whether nothing is currently listening on `port` (on any interface). */
function available(port: number): Promise<boolean> {
  return new Promise((resolve) => {
    const server = createServer();
    server.once("error", () => resolve(false));
    server.once("listening", () => server.close(() => resolve(true)));
    server.listen(port, "0.0.0.0");
  });
}

/**
 * First available port at or above `preferred`. Used so the gateway falls back
 * to a free host port when the default is already taken.
 */
export async function findAvailablePort(preferred: number): Promise<number> {
  for (let port = preferred; port < preferred + 1000; port++) {
    if (await available(port)) return port;
  }
  throw new Error(`no free port found near ${preferred}`);
}
