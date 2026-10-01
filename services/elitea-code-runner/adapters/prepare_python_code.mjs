// Trusted package-preparation entry point. It never executes the supplied Code.
import { loadPyodide } from "npm:pyodide@0.29.0";
import { preparePythonPackages } from "./preload.mjs";
import { discoverPythonRequirements } from "./python_requirements.mjs";

const encoder = new TextEncoder();
const MAX_FILE_BYTES = 32 * 1024 * 1024;
const MAX_BUNDLE_BYTES = 128 * 1024 * 1024;
const LOCK_NAME = "elitea-python-lock.json";
const BUNDLE_NAME = "elitea-python-bundle.json";

async function digest(bytes) {
  return Array.from(
    new Uint8Array(await crypto.subtle.digest("SHA-256", bytes)),
  )
    .map((byte) => byte.toString(16).padStart(2, "0")).join("");
}
function safeName(name, pattern) {
  return typeof name === "string" && name.length <= 256 &&
    pattern.exec(name)?.[0] === name;
}
function validDigest(value) {
  return typeof value === "string" && value.length === 64 &&
    /^[a-f0-9]+$/.test(value);
}
async function readRegular(path, maximum) {
  const stat = await Deno.lstat(path);
  if (!stat.isFile || stat.size > maximum) {
    throw new Error("Prepared package content requires a bounded regular file");
  }
  using file = await Deno.open(path, { read: true });
  const opened = await file.stat();
  if (!opened.isFile || opened.size !== stat.size || opened.ino !== stat.ino) {
    throw new Error("Prepared package content changed during verification");
  }
  const bytes = new Uint8Array(stat.size);
  let offset = 0;
  while (offset < bytes.length) {
    const count = await file.read(bytes.subarray(offset));
    if (count === null) {
      throw new Error("Prepared package content changed during verification");
    }
    offset += count;
  }
  if (await file.read(new Uint8Array(1)) !== null) {
    throw new Error("Prepared package content changed beyond its size bound");
  }
  return bytes;
}
async function fileEntry(directory, name) {
  if (!safeName(name, /^[A-Za-z0-9_.+-]+\.(whl|json)$/)) {
    throw new Error("Prepared package content requires a safe file name");
  }
  const path = `${directory}/${name}`;
  const maximum = name === LOCK_NAME ? 1024 * 1024 : MAX_FILE_BYTES;
  const bytes = await readRegular(path, maximum);
  return { name, bytes: bytes.length, sha256: await digest(bytes) };
}
async function inventory(directory, lock) {
  const names = new Set([LOCK_NAME]);
  const integrity = new Map();
  for (const entry of Object.values(lock.packages)) {
    if (
      !safeName(entry.file_name, /^[A-Za-z0-9_.+-]+\.whl$/) ||
      !validDigest(entry.sha256)
    ) {
      throw new Error(
        "Prepared Python lock contains invalid content references",
      );
    }
    if (
      integrity.has(entry.file_name) &&
      integrity.get(entry.file_name) !== entry.sha256
    ) {
      throw new Error("Prepared Python lock contains conflicting content");
    }
    integrity.set(entry.file_name, entry.sha256);
    names.add(entry.file_name);
  }
  if (names.size > 257) {
    throw new Error("Prepared Python bundle exceeds its file-count limit");
  }
  const files = [];
  let total = 0;
  for (const name of [...names].sort()) {
    const entry = await fileEntry(directory, name);
    total += entry.bytes;
    if (total > MAX_BUNDLE_BYTES) {
      throw new Error("Prepared Python bundle exceeds 128 MiB");
    }
    if (integrity.has(name) && integrity.get(name) !== entry.sha256) {
      throw new Error("Prepared Python wheel does not match the native lock");
    }
    files.push(entry);
  }
  return files;
}

export async function preparePythonCodePackages(directory, source) {
  try {
    await Deno.lstat(`${directory}/${BUNDLE_NAME}`);
    throw new Error(
      "Resolved bundles require verified reuse, not preparation in place",
    );
  } catch (error) {
    if (!(error instanceof Deno.errors.NotFound)) throw error;
  }
  const python = await loadPyodide({ packageCacheDir: directory });
  const plan = discoverPythonRequirements(python, source);
  if (plan.dynamic_installs) {
    throw new Error(
      "Dynamic package expressions are unsupported; use literal requirements in micropip.install",
    );
  }
  const lock = await preparePythonPackages(directory, plan.requirements, {
    requireExactVersions: false,
    interpreter: python,
  });
  const content = {
    revision: 1,
    runtime: "pyodide-0.29.0",
    requirements: plan.requirements,
    files: await inventory(directory, lock),
  };
  const bundle = {
    ...content,
    digest: await digest(encoder.encode(JSON.stringify(content))),
  };
  await Deno.writeTextFile(
    `${directory}/${BUNDLE_NAME}`,
    JSON.stringify(bundle),
    { createNew: true },
  );
  return bundle;
}

// Receipt reuse validates every byte. It never performs registry resolution.
export async function verifyPythonCodePackages(directory, expectedDigest) {
  if (
    !validDigest(expectedDigest)
  ) {
    throw new Error(
      "Bundle reuse requires its previously recorded content digest",
    );
  }
  const decoder = new TextDecoder("utf-8", { fatal: true });
  const bundle = JSON.parse(decoder.decode(
    await readRegular(`${directory}/${BUNDLE_NAME}`, 128 * 1024),
  ));
  if (bundle.revision !== 1 || bundle.runtime !== "pyodide-0.29.0") {
    throw new Error("Prepared Python bundle has an unsupported revision");
  }
  const lock = JSON.parse(decoder.decode(
    await readRegular(`${directory}/${LOCK_NAME}`, 1024 * 1024),
  ));
  const files = await inventory(directory, lock);
  const content = {
    revision: bundle.revision,
    runtime: bundle.runtime,
    requirements: bundle.requirements,
    files,
  };
  if (
    !Array.isArray(bundle.requirements) || bundle.requirements.length > 128 ||
    bundle.requirements.some((value) =>
      typeof value !== "string" || value.length === 0 || value.length > 256 ||
      !/^[\x20-\x7e]+$/.test(value)
    ) ||
    bundle.digest !== expectedDigest ||
    JSON.stringify(bundle.files) !== JSON.stringify(files) ||
    await digest(encoder.encode(JSON.stringify(content))) !== bundle.digest
  ) {
    throw new Error("Prepared Python bundle content changed after resolution");
  }
  return bundle;
}

if (import.meta.main) {
  if (Deno.args.length === 3 && Deno.args[0] === "--verify") {
    console.log(JSON.stringify(
      await verifyPythonCodePackages(Deno.args[1], Deno.args[2]),
    ));
  } else if (Deno.args.length === 2) {
    const [directory, sourcePath] = Deno.args;
    const bundle = await preparePythonCodePackages(
      directory,
      new TextDecoder("utf-8", { fatal: true }).decode(
        await readRegular(sourcePath, 256 * 1024),
      ),
    );
    console.log(JSON.stringify(bundle));
  } else {
    throw new Error(
      "Use <private-directory> <source-file>, or --verify <directory> <recorded-digest>",
    );
  }
}
