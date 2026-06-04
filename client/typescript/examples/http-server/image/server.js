const http = require("http");

const server = http.createServer((req, res) => {
  const chunks = [];
  req.on("data", (chunk) => chunks.push(chunk));
  req.on("end", () => {
    const body = Buffer.concat(chunks).toString();

    const echo = {
      method: req.method,
      url: req.url,
      headers: req.headers,
      body: body || undefined,
    };

    const payload = JSON.stringify(echo, null, 2);
    res.writeHead(200, {
      "content-type": "application/json",
      "content-length": Buffer.byteLength(payload),
    });
    res.end(payload);
  });
});

server.listen(8080, () => {
  console.log("http-echo server listening on :8080");
});

const server2 = http.createServer((req, res) => {
  const payload = JSON.stringify({ port: 9000, url: req.url }, null, 2);
  res.writeHead(200, { "content-type": "application/json", "content-length": Buffer.byteLength(payload) });
  res.end(payload);
});

server2.listen(9000, () => {
  console.log("http-echo server listening on :9000");
});
