// Low-level readings, from the OS rather than the JS engine.
//
// Windows: polyvisor.node (native/polyvisor.cc) reads the process's private bytes, its job object (the
// service's limit under polyvisor or the launcher) and the system's memory. Without the addon, process and
// system memory come from Node's own APIs and the job is unknown (the POLYVISOR_* env still gives limits).
// Linux: cgroup v2 files (memory.current/max/high/peak, cpu.stat) and /proc/self/status, no addon needed.
import { createRequire } from "node:module";
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import os from "node:os";

/**
 * @typedef {object} ProcessMemory
 * @property {number} privateBytes committed memory: what a job's process limit counts
 * @property {number} workingSetBytes resident
 */
/**
 * @typedef {object} JobReading
 * @property {number} memoryLimitBytes the tighter of the job's process and job limits (0: none)
 * @property {number} memoryBytes what the job (the service's whole process tree) has committed
 * @property {number} peakMemoryBytes
 * @property {number} processes
 * @property {string} kind "job" | "cgroup"
 */
/**
 * @typedef {object} SystemMemory
 * @property {number} totalBytes
 * @property {number} availableBytes
 * @property {number} loadPercent
 */

const here = dirname(fileURLToPath(import.meta.url));

/** @type {any} */
let addon = null;
/** @type {string | null} */
let addonError = null;
if (process.platform === "win32") {
  try {
    const require = createRequire(import.meta.url);
    addon = require(process.env.POLYVISOR_ADDON || join(here, "polyvisor.node"));
  } catch (err) {
    addonError = err instanceof Error ? err.message : String(err);
  }
}

/** where the readings come from: "addon", "cgroup" or "node" (engine APIs, less exact) */
export const backend = addon ? "addon" : process.platform === "linux" && cgroupDir() ? "cgroup" : "node";
export const nativeError = addonError;

/** @returns {ProcessMemory} */
export function processMemory() {
  if (addon) {
    const m = addon.processMemory();
    if (m) return { privateBytes: m.privateBytes, workingSetBytes: m.workingSetBytes };
  }
  if (process.platform === "linux") {
    // VmRSS is resident; RssAnon + VmSwap is the private part a cgroup charges
    const status = read("/proc/self/status");
    if (status) {
      const kb = (/** @type {string} */ k) => Number(new RegExp(`^${k}:\\s+(\\d+)`, "m").exec(status)?.[1] ?? 0) * 1024;
      return { privateBytes: kb("RssAnon") + kb("VmSwap"), workingSetBytes: kb("VmRSS") };
    }
  }
  const rss = process.memoryUsage.rss();
  return { privateBytes: rss, workingSetBytes: rss };
}

/** @returns {JobReading | null} the job/cgroup this process runs in, or null when it isn't limited by one */
export function job() {
  if (addon) {
    const name = process.env.POLYVISOR_JOB || process.env.WIN_TELEMETRY_JOB;
    const j = (name && addon.job(name)) || addon.job();
    if (!j) return null;
    const limits = [j.processMemoryLimitBytes, j.jobMemoryLimitBytes].filter((v) => v > 0);
    return {
      kind: "job",
      memoryLimitBytes: limits.length ? Math.min(...limits) : 0,
      memoryBytes: j.jobMemoryBytes ?? 0,
      peakMemoryBytes: Math.max(j.peakJobMemoryBytes ?? 0, j.peakProcessMemoryBytes ?? 0),
      processes: j.activeProcesses ?? 0,
    };
  }
  const dir = cgroupDir();
  if (!dir) return null;
  const max = read(join(dir, "memory.max"))?.trim();
  return {
    kind: "cgroup",
    memoryLimitBytes: max && max !== "max" ? Number(max) : 0,
    memoryBytes: Number(read(join(dir, "memory.current")) ?? 0),
    peakMemoryBytes: Number(read(join(dir, "memory.peak")) ?? 0),
    processes: (read(join(dir, "cgroup.procs")) ?? "").split("\n").filter(Boolean).length,
  };
}

/** @returns {SystemMemory} */
export function systemMemory() {
  if (addon) {
    const s = addon.systemMemory();
    if (s) return { totalBytes: s.totalBytes, availableBytes: s.availableBytes, loadPercent: s.loadPercent };
  }
  let total = os.totalmem();
  let available = os.freemem();
  if (process.platform === "linux") {
    const info = read("/proc/meminfo");
    const kb = (/** @type {string} */ k) => Number(new RegExp(`^${k}:\\s+(\\d+)`, "m").exec(info ?? "")?.[1] ?? NaN) * 1024;
    if (!Number.isNaN(kb("MemAvailable"))) available = kb("MemAvailable");
    if (!Number.isNaN(kb("MemTotal"))) total = kb("MemTotal");
  }
  return { totalBytes: total, availableBytes: available, loadPercent: Math.round((1 - available / total) * 100) };
}

/** @returns {number} this process's CPU time so far, in microseconds */
export function cpuMicros() {
  if (addon) {
    const t = addon.cpuTimes();
    if (t) return (t.userNanos + t.kernelNanos) / 1000;
  }
  const u = process.cpuUsage();
  return u.user + u.system;
}

/** @type {string | null | undefined} */
let cgroup;
function cgroupDir() {
  if (cgroup !== undefined) return cgroup;
  cgroup = null;
  if (process.platform !== "linux") return cgroup;
  const fromEnv = process.env.POLYVISOR_CGROUP;
  if (fromEnv) return (cgroup = fromEnv);
  const line = read("/proc/self/cgroup")?.split("\n").find((l) => l.startsWith("0::"));
  if (line) cgroup = join("/sys/fs/cgroup", line.slice(3).trim());
  return cgroup;
}

/** @param {string} path */
function read(path) {
  try {
    return readFileSync(path, "utf8");
  } catch {
    return null;
  }
}
