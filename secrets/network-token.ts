// Prints a fresh NETWORK_TOKEN, in the format every service expects, without touching any file:
//
//   cd secrets && bun run network-token
//
// Format: 64 lowercase hex characters, the SHA-256 of 32 random bytes (generate.ts makes its tokens the
// same way). The network service refuses anything shorter than 32 characters; hex keeps the value safe
// unquoted in any .env file, header or URL. Example (don't use it):
//
//   NETWORK_TOKEN=3f9c2a7e0b4d61c85e2f9a0b7c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f
//
// The same value goes into every service that joins the network: api/.env, billing/.env, importer/.env,
// vault/.env (optional) and network/.env, or secrets/secrets.env for docker compose. To change the one
// generate.ts wrote, run `bun generate.ts --rotate network` instead, then restart the services.
import { novoToken } from "./generate";

const FORMATO = /^[0-9a-f]{64}$/;

const token = await novoToken();
if (!FORMATO.test(token)) throw new Error("unexpected token format");

if (process.argv.includes("--check")) {
  // bun run network-token --check <value>: says whether a value has the expected format
  const valor = process.argv[process.argv.indexOf("--check") + 1] ?? "";
  const ok = FORMATO.test(valor);
  console.log(ok ? "ok: 64 hex characters" : "not the expected format: 64 lowercase hex characters (SHA-256 of 32 random bytes)");
  process.exit(ok ? 0 : 1);
}

console.log(`NETWORK_TOKEN=${token}`);
console.error("paste the line above into the .env of every service that joins the network (the same value everywhere)");
