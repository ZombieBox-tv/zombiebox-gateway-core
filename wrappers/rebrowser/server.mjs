import { webURL, allowed } from "./navigation-policy.mjs";
import http from "node:http";
import { readFile, mkdtemp, rm } from "node:fs/promises";
import { timingSafeEqual } from "node:crypto";
import puppeteer from "puppeteer-core";

const config = JSON.parse(
  await readFile(process.env.ZOMBIE_BROWSER_CONFIG || "/config/browser.json", "utf8"),
);
if (typeof config.token !== "string" || config.token.length < 32)
  throw Error("Invalid private worker configuration");
let session = null,
  busy = false;
const idPattern = /^[a-f0-9]{32}$/;
async function close() {
  const old = session;
  session = null;
  if (old) {
    await old.browser.close().catch(() => {});
    await rm(old.directory, { recursive: true, force: true });
  }
}
async function start(id, url) {
  if (!idPattern.test(id) || !(await allowed(url))) throw Error("Invalid session");
  const directory = await mkdtemp("/tmp/zombie-browser-");
  let browser;
  try {
    browser = await puppeteer.launch({
      executablePath: "/usr/bin/chromium-browser",
      headless: true,
      timeout: 8000,
      userDataDir: directory,
      args: [
        "--disable-setuid-sandbox",
        "--disable-dev-shm-usage",
        "--disable-background-networking",
        "--disable-extensions",
        "--disable-sync",
        "--no-first-run",
        "--force-webrtc-ip-handling-policy=disable_non_proxied_udp",
      ],
      defaultViewport: { width: 960, height: 540, deviceScaleFactor: 1 },
    });
    const page = await browser.newPage();
    page.setDefaultTimeout(5000);
    page.setDefaultNavigationTimeout(5000);
    await page.setRequestInterception(true);
    page.on("request", (request) => {
      void (async () => {
        try {
          if (await allowed(request.url())) await request.continue();
          else await request.abort();
        } catch {}
      })();
    });
    page.on("popup", (popup) => {
      void popup.close();
    });
    page.on("dialog", (dialog) => {
      void dialog.dismiss();
    });
    const cdp = await page.createCDPSession();
    await cdp.send("Browser.setDownloadBehavior", { behavior: "deny" });
    await cdp.detach();
    await page.goto(url, { waitUntil: "domcontentloaded" });
    session = { id, browser, page, directory, touched: Date.now() };
  } catch (error) {
    await browser?.close().catch(() => {});
    await rm(directory, { recursive: true, force: true });
    throw error;
  }
}
async function body(req) {
  const chunks = [];
  let size = 0;
  for await (const chunk of req) {
    size += chunk.length;
    if (size > 8192) throw Error("Request too large");
    chunks.push(chunk);
  }
  return JSON.parse(Buffer.concat(chunks).toString("utf8") || "{}");
}
function json(res, status, value) {
  res.writeHead(status, { "Content-Type": "application/json", "Cache-Control": "no-store" });
  res.end(JSON.stringify(value));
}
const server = http.createServer(async (req, res) => {
  res.setHeader("X-Content-Type-Options", "nosniff");
  res.setHeader("Cache-Control", "no-store");
  if (req.method === "GET" && req.url === "/health") {
    json(res, 200, { available: true, active: session !== null });
    return;
  }
  const supplied = Buffer.from(req.headers.authorization || ""),
    expected = Buffer.from("Bearer " + config.token);
  if (supplied.length !== expected.length || !timingSafeEqual(supplied, expected)) {
    json(res, 401, { error: "unauthorized" });
    return;
  }
  if (busy) {
    json(res, 429, { error: "busy" });
    return;
  }
  busy = true;
  try {
    if (req.method === "POST" && req.url === "/session") {
      if (session) {
        json(res, 409, { error: "session_busy" });
        return;
      }
      const data = await body(req);
      await start(data.id, data.url);
      if (res.destroyed) {
        await close();
        return;
      }
      json(res, 201, { sessionId: session.id });
      return;
    }
    const match = req.url.match(/^\/session\/([a-f0-9]{32})(\/frame|\/input)?$/);
    if (!match || !session || match[1] !== session.id) {
      json(res, 404, { error: "session_not_found" });
      return;
    }
    session.touched = Date.now();
    if (req.method === "DELETE" && !match[2]) {
      await close();
      json(res, 200, { closed: true });
      return;
    }
    if (req.method === "GET" && match[2] === "/frame") {
      const image = await session.page.screenshot({
        type: "jpeg",
        quality: 65,
        optimizeForSpeed: true,
      });
      if (image.length > 1024 * 1024) throw Error("Frame too large");
      res.writeHead(200, { "Content-Type": "image/jpeg", "Content-Length": image.length });
      res.end(image);
      return;
    }
    if (req.method === "POST" && match[2] === "/input") {
      const command = await body(req),
        page = session.page;
      if (command.action === "navigate") {
        if (!(await allowed(command.text))) throw Error("Invalid URL");
        await page.goto(command.text, { waitUntil: "domcontentloaded" });
      } else if (
        command.action === "text" &&
        typeof command.text === "string" &&
        command.text.length <= 2000
      )
        await page.keyboard.insertText(command.text);
      else if (
        command.action === "key" &&
        [
          "Tab",
          "Shift+Tab",
          "Enter",
          "Escape",
          "ArrowUp",
          "ArrowDown",
          "ArrowLeft",
          "ArrowRight",
          "PageUp",
          "PageDown",
          "Backspace",
        ].includes(command.text)
      )
        await page.keyboard.press(command.text);
      else if (command.action === "back") await page.goBack({ waitUntil: "domcontentloaded" });
      else if (command.action === "forward")
        await page.goForward({ waitUntil: "domcontentloaded" });
      else if (command.action === "reload") await page.reload({ waitUntil: "domcontentloaded" });
      else throw Error("Invalid command");
      json(res, 200, { accepted: true });
      return;
    }
    json(res, 405, { error: "method_not_allowed" });
  } catch {
    json(res, 502, { error: "browser_unavailable" });
  } finally {
    busy = false;
  }
});
server.requestTimeout = 10000;
server.headersTimeout = 5000;
server.maxConnections = 8;
const reaper = setInterval(() => {
  if (session && !busy && Date.now() - session.touched > 90000) {
    busy = true;
    void close().finally(() => {
      busy = false;
    });
  }
}, 5000);
server.listen(8094, "0.0.0.0");
for (const signal of ["SIGTERM", "SIGINT"])
  process.on(signal, () => {
    clearInterval(reaper);
    server.close();
    void close().finally(() => process.exit(0));
  });
