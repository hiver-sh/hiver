#!/usr/bin/env node

let data = "";
process.stdin.on("data", (c) => (data += c)).on("end", () => {
  let msg = data;
  try {
    const j = JSON.parse(data);
    msg = j.message || JSON.stringify(j);
  } catch (e) {}
  require("fs").appendFileSync("/run/stdout", `[claude notification] ${msg}\n`);
});
