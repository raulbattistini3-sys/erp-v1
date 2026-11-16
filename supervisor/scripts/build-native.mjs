// Builds native/polyvisor.cc into src/runtime/polyvisor.node (Windows only: on Linux the runtime reads
// cgroup v2 and /proc files directly and needs no addon).
//
// No node-gyp and no node.lib: the addon resolves the Node-API functions from the host process (Node or
// Bun) when it loads, so it only needs the headers (node-api-headers) and a compiler. MSVC's cl.exe when it
// is on PATH (a Developer PowerShell), otherwise MinGW-w64 g++ (CXX=... picks another).
import { execFileSync, spawnSync } from "node:child_process";
import { mkdirSync } from "node:fs";
import { createRequire } from "node:module";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

if (process.platform !== "win32") {
  console.log("polyvisor: not Windows, no addon to build");
  process.exit(0);
}

const root = join(dirname(fileURLToPath(import.meta.url)), "..");
const require = createRequire(import.meta.url);
let include;
try {
  include = join(dirname(require.resolve("node-api-headers/package.json")), "include");
} catch {
  console.error("polyvisor: node-api-headers is missing (npm install)");
  process.exit(1);
}
const source = join(root, "native", "polyvisor.cc");
const outDir = join(root, "build");
const out = join(root, "src", "runtime", "polyvisor.node");
mkdirSync(outDir, { recursive: true });

const found = (cmd) => spawnSync("where", [cmd], { stdio: "ignore" }).status === 0;
const libs = ["psapi", "kernel32"];

let cmd, args;
if (!process.env.CXX && found("cl")) {
  cmd = "cl";
  args = ["/nologo", "/LD", "/O2", "/EHsc", "/std:c++17", "/MT", `/I${include}`, source,
    `/Fe:${out}`, `/Fo:${outDir}\\`, "/link", ...libs.map((l) => `${l}.lib`)];
} else {
  cmd = process.env.CXX || "g++";
  if (!process.env.CXX && !found("g++")) {
    console.error("polyvisor: no C++ compiler (cl.exe from a Developer PowerShell, MinGW-w64 g++ on PATH, or CXX=...)");
    process.exit(1);
  }
  // static runtime: the .node needs system DLLs only
  args = ["-shared", "-O2", "-std=c++17", "-Wall", "-Wno-unused-parameter", `-I${include}`,
    source, "-o", out, "-static", "-static-libgcc", "-static-libstdc++", ...libs.map((l) => `-l${l}`)];
}

console.log(`polyvisor: ${cmd} ${args.join(" ")}`);
try {
  execFileSync(cmd, args, { stdio: "inherit" });
} catch {
  process.exit(1);
}
console.log(`polyvisor: built ${out}`);
