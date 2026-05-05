// Node.js port of agent-python's agent.py. Runs the same probe
// sequence so the proxy and FUSE assertions in expectations.yaml work
// against either fixture; the only on-stdout difference is that the
// "GET" / "POST" / "CURL GET" intent lines come from JavaScript code
// instead of Python's stdlib.

const http = require("http");
const https = require("https");
const fs = require("fs");
const { spawnSync } = require("child_process");


const server = http.createServer((req, res) => {
  if (req.method !== "POST") {
    res.writeHead(405);
    res.end();
    return;
  }
  const chunks = [];
  req.on("data", (c) => chunks.push(c));
  req.on("end", () => {
    const body = Buffer.concat(chunks).toString("utf8");
    if (req.url === "/exec") {
      handleExec(body, res);
      return;
    }
    console.log(`INGRESS POST ${req.url} '${body}'`);
    const okBody = "ok\n";
    res.writeHead(200, { "Content-Length": Buffer.byteLength(okBody) });
    res.end(okBody);
  });
});

function handleExec(command, res) {
  let payload;
  try {
    const r = spawnSync("/bin/bash", ["-c", command], {
      encoding: "utf8",
      timeout: 30_000,
    });
    payload = {
      exit_code: r.status === null ? -1 : r.status,
      stdout: r.stdout || "",
      stderr: r.stderr || "",
    };
  } catch (e) {
    payload = { exit_code: -1, stdout: "", stderr: String(e) };
  }
  console.log(`INGRESS EXEC '${command}' → exit=${payload.exit_code}`);
  const json = JSON.stringify(payload);
  res.writeHead(200, {
    "Content-Type": "application/json",
    "Content-Length": Buffer.byteLength(json),
  });
  res.end(json);
}

server.listen(18000, "0.0.0.0");

// ============ Outbound probes ============
function httpGet(url) {
  console.log(`GET ${url}`);
  return new Promise((resolve) => {
    const lib = url.startsWith("https") ? https : http;
    // agent: false disables keep-alive — the proxy is one-request-per-
    // connection so any reuse would silently stall the next probe.
    const req = lib.get(url, { timeout: 5000, agent: false }, (res) => {
      res.resume();
      res.on("end", resolve);
    });
    req.on("error", () => resolve());
    req.on("timeout", () => {
      req.destroy();
      resolve();
    });
  });
}

function httpPost(url) {
  console.log(`POST ${url}`);
  return new Promise((resolve) => {
    const u = new URL(url);
    const lib = u.protocol === "https:" ? https : http;
    const req = lib.request(
      {
        method: "POST",
        hostname: u.hostname,
        port: u.port,
        path: u.pathname + u.search,
        timeout: 5000,
        agent: false,
      },
      (res) => {
        res.resume();
        res.on("end", resolve);
      },
    );
    req.on("error", () => resolve());
    req.on("timeout", () => {
      req.destroy();
      resolve();
    });
    req.write("x");
    req.end();
  });
}

// curlGet exercises a separate TLS stack (libcurl + OpenSSL) than
// Node's built-in tls module. Both go through the same iptables
// REDIRECT + sbxproxy MITM, but curl's stricter cert validation
// surfaces issues (untrusted CA, SNI mismatches) that Node might miss.
function curlGet(url) {
  console.log(`CURL GET ${url}`);
  spawnSync("curl", ["-sS", "-o", "/dev/null", "--max-time", "5", url]);
}

function fsWrite(path, content) {
  console.log(`WRITE ${path}`);
  try {
    fs.writeFileSync(path, content);
  } catch (_) {}
}

function fsRead(path) {
  console.log(`READ ${path}`);
  try {
    fs.readFileSync(path);
  } catch (_) {}
}

(async () => {
  // Probes match the egress rules in spec.yaml: the proxy allows GET /
  // on upstream-allowed (with a header override) and TLS to
  // go.dev/solutions/case-studies (intercepted, path-matched).
  await httpGet("http://upstream-allowed:17080/");
  await httpPost("http://upstream-allowed:17080/");
  await httpGet("http://upstream-allowed:17080/forbidden");
  await httpGet("http://upstream-denied:17081/");
  curlGet("https://go.dev/solutions/case-studies/");
  curlGet("https://go.dev/doc/devel/release");

  // /workspace is a FUSE mount — sbxfuse mediates per-op via ACLs.
  fsWrite("/workspace/hello.txt", "hello from node");
  fsRead("/workspace/hello.txt");
  fsRead("/workspace/secret/keys.txt");

  // /workspace/inputs/* is read-only (ACL ro) and pre-seeded by sandboxd.
  // Read succeeds; the write attempt is rejected at FUSE Open.
  fsRead("/workspace/inputs/data.txt");
  fsWrite("/workspace/inputs/data.txt", "trying to overwrite");

  console.log("DONE");
})();
// The HTTP server keeps the event loop alive — no explicit sleep loop.
