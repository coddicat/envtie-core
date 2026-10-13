#!/usr/bin/env node
import { chmodSync } from "node:fs";
import { createRequire } from "node:module";
import { spawnSync } from "node:child_process";

const require = createRequire(import.meta.url);
const target = `${process.platform}-${process.arch}`;
const packages = {
  "darwin-arm64": "@envtie.dev/cli-darwin-arm64",
  "darwin-x64": "@envtie.dev/cli-darwin-x64",
  "linux-arm64": "@envtie.dev/cli-linux-arm64",
  "linux-x64": "@envtie.dev/cli-linux-x64",
  "win32-arm64": "@envtie.dev/cli-win32-arm64",
  "win32-x64": "@envtie.dev/cli-win32-x64",
};

const packageName = packages[target];
if (!packageName) {
  console.error(`envtie: unsupported platform ${target}`);
  process.exit(1);
}

let binary;
try {
  binary = process.env.ENVTIE_BINARY_PATH || require.resolve(`${packageName}/envtie${process.platform === "win32" ? ".exe" : ""}`);
} catch {
  console.error(
    `envtie: ${packageName} is missing. Reinstall without --omit=optional, or use the EnvTie.dev container image.`,
  );
  process.exit(1);
}

if (process.platform !== "win32") {
  try {
    chmodSync(binary, 0o755);
  } catch {
    // A read-only install still fails at spawn with a clear error.
  }
}

const args = process.argv.slice(2);
if (args[0] === "runner" && args[1] === "start") args[1] = "execute";
const apiIndex = args.indexOf("--api-url");
if (apiIndex >= 0) {
  const value = args[apiIndex + 1];
  if (!value || value.startsWith("-")) {
    console.error("envtie: --api-url needs a value");
    process.exit(2);
  }
  process.env.ENVTIE_API_URL = value;
  args.splice(apiIndex, 2);
}

const result = spawnSync(binary, args, { env: process.env, stdio: "inherit" });
if (result.error) {
  console.error(`envtie: could not start the native runner (${result.error.code || "spawn_failed"})`);
  process.exit(1);
}
if (result.signal) process.kill(process.pid, result.signal);
process.exit(result.status ?? 1);
