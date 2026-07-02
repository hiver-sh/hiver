// Minimal ambient declaration for the `ws` package. We use `ws` (not Node's
// native/global WebSocket) because the gateway's WebSocket proxy rejects the
// undici client's upgrade with a non-101 status, whereas `ws` connects fine —
// the same reason skills/browser/cdp-bridge.js uses it. `ws` ships no bundled
// types and @types/ws isn't installed, so we declare just the browser-style
// surface cdp.ts uses. Drop this if @types/ws is added.
declare module "ws" {
  export default class WebSocket {
    constructor(url: string);
    readyState: number;
    send(data: string): void;
    close(): void;
    addEventListener(type: "open", cb: () => void): void;
    addEventListener(type: "close", cb: () => void): void;
    addEventListener(type: "error", cb: (ev: { message?: string }) => void): void;
    addEventListener(
      type: "message",
      cb: (ev: { data: unknown }) => void,
    ): void;
  }
}
