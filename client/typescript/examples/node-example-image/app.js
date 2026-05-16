const http = require("http");
const https = require("https");
const fs = require("fs");
const { spawnSync } = require("child_process");


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
  // Denied — no allow rule matches. Each should produce an
  // `egress.request` event with `access: "denied"`.
  //
  // Use real, resolvable hosts. The proxy is transparent: it only
  // sees TCP connections after the agent's DNS lookup succeeds. The
  // e2e fixture uses `upstream-allowed` / `upstream-denied` because
  // it wires them up with `--add-host`; a controller-spawned sandbox
  // doesn't, so probes against those names fail at DNS and never
  // reach the proxy (hence: no event).
  await httpGet("http://example.com/");
  await httpPost("http://example.com/submit");
  curlGet("https://www.google.com/search?q=Carmel,+California");

  // Allowed — matches the `host: www.npmjs.com` rule the example
  // installs. Raw-forward TLS emits one `egress.request` with
  // `access: "allowed"`; no `egress.response` follows because the
  // proxy never sees the inner HTTP status under raw-forward TLS.
  curlGet("https://www.npmjs.com/");

  fsWrite("/workspace/hello.txt", "hello from node");
  fsRead("/workspace/hello.txt");
  fsRead("/workspace/secret/keys.txt");
  fsRead("/workspace/inputs/data.txt");
  fsWrite("/workspace/inputs/data.txt", "trying to overwrite");

  console.log("DONE");
})();
