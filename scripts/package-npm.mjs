#!/usr/bin/env node
import fs from "node:fs";
import path from "node:path";
import process from "node:process";

const [tag, binaryDir = "dist", outputDir = "dist/npm"] = process.argv.slice(2);
if (!/^v\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?$/.test(tag || "")) {
  throw new Error("usage: node scripts/package-npm.mjs v1.2.3 [binary-dir] [output-dir]");
}
const version = tag.slice(1);
// npm provenance rejects a package whose repository differs from the publishing workflow's.
const repository = {
  type: "git",
  url: `git+https://github.com/${process.env.GITHUB_REPOSITORY || "coddicat/envtie-core"}.git`,
};
const root = path.resolve(import.meta.dirname, "..");
const source = path.join(root, "packages", "cli");
const out = path.resolve(root, outputDir);
fs.rmSync(out, { force: true, recursive: true });
fs.mkdirSync(out, { recursive: true });

const targets = [
  ["darwin", "amd64", "darwin", "x64", ""],
  ["darwin", "arm64", "darwin", "arm64", ""],
  ["linux", "amd64", "linux", "x64", ""],
  ["linux", "arm64", "linux", "arm64", ""],
  ["windows", "amd64", "win32", "x64", ".exe"],
  ["windows", "arm64", "win32", "arm64", ".exe"],
];

const mainDir = path.join(out, "cli");
fs.cpSync(source, mainDir, { recursive: true });
const mainPackagePath = path.join(mainDir, "package.json");
const mainPackage = JSON.parse(fs.readFileSync(mainPackagePath, "utf8"));
mainPackage.version = version;
mainPackage.repository = repository;
for (const name of Object.keys(mainPackage.optionalDependencies)) {
  mainPackage.optionalDependencies[name] = version;
}
fs.writeFileSync(mainPackagePath, `${JSON.stringify(mainPackage, null, 2)}\n`);

for (const [goos, goarch, nodeos, nodearch, ext] of targets) {
  const packageName = `@envtie.dev/cli-${nodeos}-${nodearch}`;
  const dir = path.join(out, `cli-${nodeos}-${nodearch}`);
  fs.mkdirSync(dir, { recursive: true });
  const asset = path.resolve(root, binaryDir, `envtie_${tag}_${goos}_${goarch}${ext}`);
  if (!fs.existsSync(asset)) throw new Error(`missing release asset ${asset}`);
  fs.copyFileSync(asset, path.join(dir, `envtie${ext}`));
  if (!ext) fs.chmodSync(path.join(dir, "envtie"), 0o755);
  const pkg = {
    name: packageName,
    version,
    description: `EnvTie.dev native runner for ${nodeos}-${nodearch}`,
    license: "Apache-2.0",
    repository,
    os: [nodeos],
    cpu: [nodearch],
    files: [`envtie${ext}`],
    publishConfig: { access: "public" },
  };
  fs.writeFileSync(path.join(dir, "package.json"), `${JSON.stringify(pkg, null, 2)}\n`);
}

console.log(`prepared npm packages for ${tag} in ${path.relative(root, out)}`);
