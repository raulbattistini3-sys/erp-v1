#!/usr/bin/env node
// The npm face of polyvisor: finds the native supervisor for this platform and hands it the command line.
// The supervisor is a Go executable (bin/<platform>-<arch>/polyvisor[.exe]); Node only launches it, so the
// services it runs don't live under a Node process. Ctrl+C reaches both through the console; this wrapper
// just waits and exits with the supervisor's code.
import { spawn } from "node:child_process";
import { binaryPath } from "../index.js";

let exe;
try {
  exe = binaryPath();
} catch (err) {
  console.error(`polyvisor: ${err instanceof Error ? err.message : err}`);
  process.exit(1);
}

const child = spawn(exe, process.argv.slice(2), { stdio: "inherit", windowsHide: false });
// the console delivers Ctrl+C to the supervisor itself; the wrapper must survive it to report the exit code
for (const sig of ["SIGINT", "SIGBREAK", "SIGHUP"]) process.on(sig, () => {});
process.on("SIGTERM", () => child.kill("SIGTERM"));
child.on("error", (err) => {
  console.error(`polyvisor: ${err.message}`);
  process.exit(1);
});
child.on("exit", (code, signal) => process.exit(code ?? (signal ? 1 : 0)));
