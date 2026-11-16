// polyvisor: programmatic access to the native supervisor (which binary, what is running) plus the
// service-side runtime (polyvisor/runtime).
import { existsSync, readFileSync } from "node:fs";
import { homedir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");

const PLATFORMS = /** @type {Record<string, string>} */ ({ win32: "windows", linux: "linux", darwin: "darwin" });
const ARCHES = /** @type {Record<string, string>} */ ({ x64: "x64", arm64: "arm64" });

/** the native supervisor for this machine (POLYVISOR_BIN overrides) */
export function binaryPath() {
  if (process.env.POLYVISOR_BIN) return process.env.POLYVISOR_BIN;
  const os = PLATFORMS[process.platform];
  const arch = ARCHES[process.arch];
  if (!os || !arch) throw new Error(`no polyvisor build for ${process.platform}-${process.arch}`);
  const exe = join(root, "bin", `${os}-${arch}`, process.platform === "win32" ? "polyvisor.exe" : "polyvisor");
  if (!existsSync(exe)) throw new Error(`${exe} is missing (npm run build:bin, or set POLYVISOR_BIN)`);
  return exe;
}

/** where running supervisors publish their control address (same rule as the Go side) */
export function home() {
  if (process.env.POLYVISOR_HOME) return process.env.POLYVISOR_HOME;
  if (process.platform === "win32" && process.env.LOCALAPPDATA) return join(process.env.LOCALAPPDATA, "polyvisor");
  return join(homedir(), ".polyvisor");
}

/**
 * the control endpoint of a running application, or null
 * @param {string} app
 * @returns {{ url: string, token: string, pid: number } | null}
 */
export function find(app) {
  try {
    const s = JSON.parse(readFileSync(join(home(), `${app}.json`), "utf8"));
    return s && s.url && s.token ? s : null;
  } catch {
    return null;
  }
}

/**
 * calls a running supervisor's control API: status(app), control(app, "restart", "api"), shutdown(app)
 * @param {string} app
 * @param {string} method
 * @param {string} path
 */
async function call(app, method, path) {
  const s = find(app);
  if (!s) throw new Error(`${app} is not running (no state in ${home()})`);
  const res = await fetch(s.url + path, { method, headers: { Authorization: `Bearer ${s.token}` } });
  const body = await res.text();
  if (!res.ok) throw new Error(`${method} ${path}: ${res.status} ${body.trim()}`);
  return body ? JSON.parse(body) : null;
}

/** @param {string} app */
export const status = (app) => call(app, "GET", "/v1/status");
/** @param {string} app @param {"start" | "stop" | "restart"} action @param {string} service */
export const control = (app, action, service) => call(app, "POST", `/v1/services/${encodeURIComponent(service)}/${action}`);
/** @param {string} app */
export const shutdown = (app) => call(app, "POST", "/v1/shutdown");

export * as runtime from "./runtime/index.js";
