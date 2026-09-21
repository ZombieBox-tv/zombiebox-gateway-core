import http from "node:http";
import net from "node:net";
import { lookup } from "node:dns/promises";
import { publicAddress, webURL } from "./navigation-policy.mjs";

// Resolve once and connect to that numeric address. A later DNS answer cannot
// replace the checked destination; HTTPS remains end-to-end inside CONNECT.
export async function destination(host, resolve = lookup) {
  let timer;
  const addresses = await Promise.race([
    resolve(host.replace(/^\[|\]$/g, ""), { all: true }),
    new Promise((_, reject) => {
      timer = setTimeout(() => reject(Error("DNS timeout")), 2500);
    }),
  ]).finally(() => clearTimeout(timer));
  if (
    !addresses.length ||
    addresses.length > 32 ||
    !addresses.every((a) => publicAddress(a.address))
  )
    throw Error("Destination denied");
  return addresses[0];
}

export async function createEgressProxy() {
  const sockets = new Set();
  function track(socket) {
    sockets.add(socket);
    socket.on("error", () => socket.destroy());
    socket.on("close", () => sockets.delete(socket));
    socket.setTimeout(30000, () => socket.destroy());
  }
  const server = http.createServer(async (request, response) => {
    let upstream;
    try {
      const url = webURL(request.url);
      if (url.protocol !== "http:") throw Error("CONNECT required");
      const address = await destination(url.hostname);
      if (request.destroyed) return;
      const headers = { ...request.headers, host: url.host };
      for (const key of (headers.connection || "").split(","))
        delete headers[key.trim().toLowerCase()];
      delete headers["proxy-authorization"];
      delete headers["proxy-connection"];
      headers.connection = "close";
      upstream = http.request(
        {
          host: address.address,
          family: address.family,
          port: Number(url.port || 80),
          method: request.method,
          path: url.pathname + url.search,
          headers,
          agent: false,
        },
        (result) => {
          response.writeHead(result.statusCode, result.headers);
          result.pipe(response);
          result.on("error", () => response.destroy());
        },
      );
      upstream.on("socket", track);
      upstream.on("error", () => {
        if (!response.headersSent) response.writeHead(502);
        response.end();
      });
      response.on("close", () => upstream.destroy());
      request.pipe(upstream);
    } catch {
      upstream?.destroy();
      response.writeHead(403);
      response.end();
    }
  });
  server.on("connect", async (request, client, head) => {
    let upstream;
    try {
      if (!/^(\[[0-9a-f:]+\]|[a-z0-9.-]+):443$/i.test(request.url)) throw Error("Invalid tunnel");
      const url = webURL("https://" + request.url);
      const address = await destination(url.hostname);
      if (client.destroyed) return;
      upstream = net.connect({ host: address.address, family: address.family, port: 443 });
      track(upstream);
      client.on("close", () => upstream.destroy());
      upstream.on("close", () => client.destroy());
      upstream.once("connect", () => {
        client.write("HTTP/1.1 200 Connection Established\r\n\r\n");
        if (head.length) upstream.write(head);
        client.pipe(upstream);
        upstream.pipe(client);
      });
    } catch {
      upstream?.destroy();
      client.end("HTTP/1.1 403 Forbidden\r\nConnection: close\r\n\r\n");
    }
  });
  server.on("connection", track);
  server.maxConnections = 32;
  server.requestTimeout = 30000;
  server.headersTimeout = 5000;
  await new Promise((resolve, reject) => {
    server.once("error", reject);
    server.listen(0, "127.0.0.1", resolve);
  });
  return {
    port: server.address().port,
    close() {
      for (const socket of sockets) socket.destroy();
      server.close();
    },
  };
}
