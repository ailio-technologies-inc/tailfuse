// Build the two Tailscale sidecars at a pinned, Go-checksum-verified release.
// Tauri supplies TAURI_ENV_TARGET_TRIPLE; a direct invocation uses the Rust host.
import { execFileSync, spawnSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

const VERSION = "1.104.7";
const BINARIES = ["tailscale", "tailscaled"];
const appDir = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const outDir = path.join(appDir, "src-tauri", "binaries");
const stamp = path.join(outDir, ".version");
const target = process.env.TAURI_ENV_TARGET_TRIPLE || execFileSync("rustc", ["--print", "host-tuple"], { encoding: "utf8" }).trim();
const extension = target.includes("windows") ? ".exe" : "";
const outputs = BINARIES.map((name) => path.join(outDir, `${name}-${target}${extension}`));
// A universal Tauri build compiles each architecture separately before it
// bundles the fat sidecar, so all three filenames are required.
const requiredOutputs = target === "universal-apple-darwin"
  ? BINARIES.flatMap((name) => ["aarch64", "x86_64", "universal"].map((architecture) =>
    path.join(outDir, `${name}-${architecture}-apple-darwin`)))
  : outputs;
const license = path.join(outDir, "tailscale-LICENSE.txt");
const modCache = execFileSync("go", ["env", "GOMODCACHE"], { encoding: "utf8" }).trim();

if (fs.existsSync(stamp) && fs.readFileSync(stamp, "utf8").trim() !== VERSION) {
  fs.rmSync(outDir, { recursive: true, force: true });
}
fs.mkdirSync(outDir, { recursive: true });

if (fs.existsSync(stamp) && requiredOutputs.every((file) => fs.existsSync(file)) && fs.existsSync(license)) {
  console.log(`Tailscale v${VERSION} sidecars already staged for ${target}`);
  process.exit(0);
}

function run(command, args, env = process.env) {
  const result = spawnSync(command, args, { stdio: "inherit", env });
  if (result.error) throw result.error;
  if (result.status !== 0) throw new Error(`${command} exited with status ${result.status}`);
}

function compile(goos, goarch) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "tailfuse-tailscale-"));
  const gopath = path.join(dir, "gopath");
  const env = {
    ...process.env,
    GOPATH: gopath,
    GOMODCACHE: modCache,
    GOOS: goos,
    GOARCH: goarch,
    CGO_ENABLED: "0",
  };
  // Go rejects GOBIN for cross-compilation. An isolated GOPATH works for both
  // native and cross builds while the normal module and build caches stay shared.
  delete env.GOBIN;
  for (const name of BINARIES) {
    run("go", [
      "install",
      "-trimpath",
      "-ldflags",
      `-X tailscale.com/version.longStamp=${VERSION} -X tailscale.com/version.shortStamp=${VERSION}`,
      `tailscale.com/cmd/${name}@v${VERSION}`,
    ], env);
  }
  return { dir, bin: path.join(gopath, "bin") };
}

function built(binary, name, goos, goarch, extension = "") {
  const native = path.join(binary.bin, name + extension);
  const cross = path.join(binary.bin, `${goos}_${goarch}`, name + extension);
  const source = [native, cross].find((file) => fs.existsSync(file));
  if (!source) throw new Error(`go install did not produce ${name} for ${goos}/${goarch}`);
  return source;
}

function arch(triple) {
  if (triple.startsWith("aarch64-")) return "arm64";
  if (triple.startsWith("x86_64-")) return "amd64";
  throw new Error(`unsupported Tailscale sidecar architecture: ${triple}`);
}

try {
  if (target === "universal-apple-darwin") {
    const arm = compile("darwin", "arm64");
    const intel = compile("darwin", "amd64");
    try {
      for (let i = 0; i < BINARIES.length; i++) {
        const name = BINARIES[i];
        const armOutput = path.join(outDir, `${name}-aarch64-apple-darwin`);
        const intelOutput = path.join(outDir, `${name}-x86_64-apple-darwin`);
        fs.copyFileSync(built(arm, name, "darwin", "arm64"), armOutput);
        fs.copyFileSync(built(intel, name, "darwin", "amd64"), intelOutput);
        run("lipo", ["-create", armOutput, intelOutput, "-output", outputs[i]]);
      }
    } finally {
      fs.rmSync(arm.dir, { recursive: true, force: true });
      fs.rmSync(intel.dir, { recursive: true, force: true });
    }
  } else {
    let goos;
    if (target.includes("apple-darwin")) goos = "darwin";
    else if (target.includes("windows")) goos = "windows";
    else if (target.includes("linux")) goos = "linux";
    else throw new Error(`unsupported Tailscale sidecar target: ${target}`);
    const goarch = arch(target);
    const binary = compile(goos, goarch);
    try {
      for (let i = 0; i < BINARIES.length; i++) {
        fs.copyFileSync(built(binary, BINARIES[i], goos, goarch, extension), outputs[i]);
      }
    } finally {
      fs.rmSync(binary.dir, { recursive: true, force: true });
    }
  }
  for (const output of requiredOutputs) fs.chmodSync(output, 0o755);

  // Module-cache files are read-only; do not propagate that mode to a file a
  // later target's staging run needs to replace.
  fs.rmSync(license, { force: true });
  fs.copyFileSync(path.join(modCache, `tailscale.com@v${VERSION}`, "LICENSE"), license);
  fs.chmodSync(license, 0o644);
  fs.writeFileSync(stamp, `${VERSION}\n`, { mode: 0o600 });
  console.log(`Staged Tailscale v${VERSION} sidecars for ${target}`);
} catch (error) {
  console.error(`Could not stage Tailscale's standalone runtime: ${error.message}`);
  process.exit(1);
}
