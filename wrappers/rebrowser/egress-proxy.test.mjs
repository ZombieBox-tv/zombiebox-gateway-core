import test from "node:test";
import assert from "node:assert/strict";
import http from "node:http";
import { destination, createEgressProxy } from "./egress-proxy.mjs";
import { publicAddress } from "./navigation-policy.mjs";

test("pins the checked numeric destination and rejects mixed DNS answers", async () => {
  let calls = 0;
  const selected = await destination("example.org", async () => {
    calls++;
    return [{ address: calls === 1 ? "93.184.215.14" : "127.0.0.1", family: 4 }];
  });
  assert.equal(calls, 1);
  assert.equal(selected.address, "93.184.215.14");
  await assert.rejects(
    destination("example.org", async () => [
      { address: "93.184.215.14" },
      { address: "127.0.0.1" },
    ]),
  );
  for (const address of [
    "::1",
    "::ffff:127.0.0.1",
    "fc00::1",
    "64:ff9b::7f00:1",
    "2002:7f00:1::",
    "2001:db8::1",
    "10.0.0.1",
    "169.254.169.254",
  ])
    assert.equal(publicAddress(address), false, address);
});

test("proxy refuses loopback HTTP and non-443 CONNECT", async () => {
  const proxy = await createEgressProxy();
  try {
    const status = await new Promise((resolve, reject) => {
      http
        .get({ host: "127.0.0.1", port: proxy.port, path: "http://127.0.0.1:80/" }, (response) => {
          response.resume();
          resolve(response.statusCode);
        })
        .on("error", reject);
    });
    assert.equal(status, 403);
    const status2 = await new Promise((resolve, reject) => {
      const request = http.request({
        host: "127.0.0.1",
        port: proxy.port,
        method: "CONNECT",
        path: "example.org:22",
      });
      request.on("connect", (response, socket) => {
        socket.destroy();
        resolve(response.statusCode);
      });
      request.on("error", reject);
      request.end();
    });
    assert.equal(status2, 403);
  } finally {
    proxy.close();
  }
});
