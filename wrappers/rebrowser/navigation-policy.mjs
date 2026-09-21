import { lookup } from "node:dns/promises";
import net from "node:net";

function publicAddress(address) {
  if (net.isIP(address) === 6) return !/^(::|fe[89ab]|f[cd])/i.test(address);
  const [a, b] = address.split(".").map(Number);
  return (
    net.isIP(address) === 4 &&
    a !== 0 &&
    a !== 10 &&
    a !== 127 &&
    a < 224 &&
    !(a === 169 && b === 254) &&
    !(a === 172 && b >= 16 && b <= 31) &&
    !(a === 192 && b === 168) &&
    !(a === 100 && b >= 64 && b <= 127) &&
    !(a === 198 && (b === 18 || b === 19))
  );
}
export function webURL(value) {
  if (typeof value !== "string" || value.length > 2048) throw Error("Invalid URL");
  const url = new URL(value);
  if (
    !["http:", "https:"].includes(url.protocol) ||
    url.username ||
    url.password ||
    (url.port && !["80", "443"].includes(url.port))
  )
    throw Error("Invalid URL");
  return url;
}
export async function allowed(value) {
  try {
    const url = webURL(value);
    let timer;
    const addresses = await Promise.race([
      lookup(url.hostname.replace(/^\[|\]$/g, ""), { all: true }),
      new Promise((_, reject) => {
        timer = setTimeout(() => reject(Error("DNS timeout")), 2500);
      }),
    ]).finally(() => clearTimeout(timer));
    return addresses.length > 0 && addresses.every((a) => publicAddress(a.address));
  } catch {
    return false;
  }
}
