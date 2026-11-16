// Cross-compiles the Go supervisor into bin/<os>-<arch>/ for the npm package.
//   node scripts/build-bin.mjs            all targets
//   node scripts/build-bin.mjs --host     this machine only
//   node scripts/build-bin.mjs --out DIR  one binary (this machine) written to DIR (the launcher's xtask)
import { execFileSync } from "node:child_process";
import { readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");
const { version } = JSON.parse(readFileSync(join(root, "package.json"), "utf8"));
const TARGETS = [
  ["windows", "amd64", "windows-x64"],
  ["windows", "arm64", "windows-arm64"],
  ["linux", "amd64", "linux-x64"],
  ["linux", "arm64", "linux-arm64"],
  ["darwin", "arm64", "darwin-arm64"],
];
const hostOs = { win32: "windows", linux: "linux", darwin: "darwin" }[process.platform];
const hostArch = { x64: "amd64", arm64: "arm64" }[process.arch];

const args = process.argv.slice(2);
const outAt = args.indexOf("--out");
let targets = TARGETS;
if (args.includes("--host") || outAt >= 0) targets = TARGETS.filter(([os, arch]) => os === hostOs && arch === hostArch);

for (const [os, arch, dir] of targets) {
  const exe = os === "windows" ? "polyvisor.exe" : "polyvisor";
  const out = outAt >= 0 ? join(resolve(args[outAt + 1]), exe) : join(root, "bin", dir, exe);
  console.log(`polyvisor: go build ${os}/${arch} → ${out}`);
  execFileSync("go", ["build", "-trimpath", "-ldflags", `-s -w -X main.version=${version}`, "-o", out, "./cmd/polyvisor"], {
    cwd: root,
    stdio: "inherit",
    env: { ...process.env, GOOS: os, GOARCH: arch, CGO_ENABLED: "0" },
  });
}
