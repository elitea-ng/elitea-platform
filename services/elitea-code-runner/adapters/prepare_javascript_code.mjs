// Trusted demand preparation. Deno parses and caches dependencies without source execution.
const encoder = new TextEncoder();
const decoder = new TextDecoder("utf-8", { fatal: true });
const RUNTIME = "deno-2.5.4";
const LOCK_NAME = "elitea-javascript-lock.json";
const MODULE_NAME = "elitea-javascript-dependencies.mjs";
const BUNDLE_NAME = "elitea-javascript-bundle.json";
const MAX_SOURCE_BYTES = 256 * 1024;
const MAX_METADATA_BYTES = 128 * 1024;
const MAX_OUTPUT_BYTES = 1024 * 1024;
const MAX_FILE_BYTES = 32 * 1024 * 1024;
const MAX_CACHE_BYTES = 128 * 1024 * 1024;
const MAX_FILES = 512;
const MAX_ENTRIES = 2048;
const MAX_REQUIREMENTS = 128;
const RULE_ID = "elitea-dependencies/record";
let activeJobs = 0;

function fail(message) {
  throw new Error(message);
}
function match(value, expression) {
  return typeof value === "string" && expression.exec(value)?.[0] === value;
}
function packageReference(value) {
  if (typeof value !== "string" || value.length > 256) return false;
  // Validate the source boundary. Deno owns package and semver resolution.
  const version = "(?:@[a-zA-Z0-9*^~<>=|.+ -]+)?";
  const path = "(?:/[a-zA-Z0-9_.-]+)*";
  return !value.split("/").some((part) => part === "." || part === "..") &&
    (match(
      value,
      new RegExp(
        "^npm:(?:@[a-z0-9][a-z0-9._-]*/)?[a-z0-9][a-z0-9._-]*" + version +
          path + "$",
      ),
    ) || match(
      value,
      new RegExp(
        "^jsr:@[a-z0-9][a-z0-9_-]*/[a-z0-9][a-z0-9_-]*" + version + path + "$",
      ),
    ));
}
function safePath(value) {
  return typeof value === "string" && value.length <= 256 &&
    match(value, /^[A-Za-z0-9_@.+/-]+$/) &&
    value.split("/").every((part) => part && part !== "." && part !== "..");
}
function validDigest(value) {
  return match(value, /^[a-f0-9]{64}$/);
}
function exactFields(value, fields) {
  return value && typeof value === "object" && !Array.isArray(value) &&
    Object.keys(value).length === fields.length &&
    fields.every((field) => Object.hasOwn(value, field));
}
async function digest(bytes) {
  return Array.from(
    new Uint8Array(await crypto.subtle.digest("SHA-256", bytes)),
  )
    .map((byte) => byte.toString(16).padStart(2, "0")).join("");
}

// The CLI loads this image-owned plugin. It never imports the supplied source.
// A single required diagnostic detects disabled, ignored, or incomplete inspection.
export default {
  name: "elitea-dependencies",
  rules: {
    record: {
      create(context) {
        const requirements = new Set();
        let rejected = false;
        const inspect = (node) => {
          const value = node?.value;
          if (match(value, /^node:[a-z][a-z0-9_/-]*$/)) return;
          if (!packageReference(value)) rejected = true;
          else requirements.add(value);
          if (requirements.size > MAX_REQUIREMENTS) rejected = true;
        };
        for (const comment of context.sourceCode.getAllComments()) {
          if (
            /deno-lint-ignore|@deno-types|@jsxImportSource|<reference\b/.test(
              comment.value,
            )
          ) rejected = true;
        }
        return {
          ImportDeclaration(node) {
            inspect(node.source);
          },
          ExportAllDeclaration(node) {
            inspect(node.source);
          },
          ExportNamedDeclaration(node) {
            if (node.source) inspect(node.source);
          },
          TSImportType(node) {
            inspect(node.argument?.literal);
          },
          ImportExpression() {
            rejected = true;
          },
          TSImportEqualsDeclaration() {
            rejected = true;
          },
          'CallExpression[callee.name="require"]'() {
            rejected = true;
          },
          "Program:exit"(node) {
            context.report({
              node,
              message: JSON.stringify({
                revision: 1,
                rejected,
                requirements: rejected ? [] : [...requirements].sort(),
              }),
            });
          },
        };
      },
    },
  },
};

async function readRegular(path, maximum) {
  const stat = await Deno.lstat(path);
  if (!stat.isFile || stat.size > maximum) {
    fail("Package content requires a bounded regular file");
  }
  using file = await Deno.open(path, { read: true });
  const opened = await file.stat();
  if (!opened.isFile || opened.ino !== stat.ino || opened.size !== stat.size) {
    fail("Package content changed during verification");
  }
  const bytes = new Uint8Array(stat.size);
  let offset = 0;
  while (offset < bytes.length) {
    const count = await file.read(bytes.subarray(offset));
    if (count === null || count === 0) {
      fail("Package content changed during verification");
    }
    offset += count;
  }
  if (await file.read(new Uint8Array(1)) !== null) {
    fail("Package content exceeds its recorded size");
  }
  return bytes;
}
async function inventory(directory, hash = true) {
  const files = [];
  let total = 0;
  let entries = 0;
  async function walk(prefix = "") {
    try {
      for await (const entry of Deno.readDir(`${directory}/${prefix}`)) {
        const name = prefix + entry.name;
        if (name === BUNDLE_NAME) continue;
        if (++entries > MAX_ENTRIES) {
          fail("Package cache exceeds its entry-count limit");
        }
        if (!safePath(name)) fail("Package cache contains an unsafe path");
        let stat;
        try {
          stat = await Deno.lstat(`${directory}/${name}`);
        } catch (error) {
          if (!hash && error instanceof Deno.errors.NotFound) continue;
          throw error;
        }
        if (stat.isDirectory) {
          if (name.split("/").length > 24) {
            fail("Package cache exceeds its directory-depth limit");
          }
          await walk(name + "/");
        } else {
          if (!stat.isFile || stat.size > MAX_FILE_BYTES) {
            fail("Package cache contains unsafe or excessive content");
          }
          total += stat.size;
          if (total > MAX_CACHE_BYTES || files.length >= MAX_FILES) {
            fail("Package cache exceeds its content limit");
          }
          files.push({
            name,
            bytes: stat.size,
            sha256: hash
              ? await digest(
                await readRegular(`${directory}/${name}`, MAX_FILE_BYTES),
              )
              : "",
          });
        }
      }
    } catch (error) {
      // Native cache writes can rename temporary files during the quota scan.
      if (hash || !(error instanceof Deno.errors.NotFound)) throw error;
    }
  }
  await walk();
  return files.sort((a, b) => a.name < b.name ? -1 : a.name > b.name ? 1 : 0);
}
function environment(directory) {
  return {
    DENO_DIR: `${directory}/deno-cache`,
    DENO_NO_UPDATE_CHECK: "1",
    DENO_NO_PROMPT: "1",
    DENO_NO_PACKAGE_JSON: "1",
    NPM_CONFIG_REGISTRY: "https://registry.npmjs.org/",
    JSR_URL: "https://jsr.io/",
    HOME: directory,
    NO_COLOR: "1",
  };
}
function deadline(options) {
  const seconds = options?.timeoutSeconds ?? 30;
  if (!Number.isSafeInteger(seconds) || seconds < 1 || seconds > 120) {
    fail("Preparation deadline is invalid");
  }
  const local = performance.now() + seconds * 1000;
  const phase = options?.deadlineMonotonicMs ?? local;
  if (!Number.isFinite(phase) || phase > local) {
    fail("Preparation phase deadline is invalid");
  }
  return { expires: phase, signal: options?.signal };
}
function remaining(until) {
  if (until.signal?.aborted) {
    throw new DOMException("Preparation cancelled", "AbortError");
  }
  const value = until.expires - performance.now();
  if (value <= 0) fail("Package preparation deadline exceeded");
  return value;
}
async function native(directory, args, until) {
  const milliseconds = remaining(until);
  const child = new Deno.Command(Deno.execPath(), {
    args,
    cwd: directory,
    clearEnv: true,
    env: environment(directory),
    stdin: "null",
    stdout: "piped",
    stderr: "piped",
  }).spawn();
  let stopped = false;
  let outputBytes = 0;
  const stop = () => {
    if (!stopped) {
      stopped = true;
      try {
        child.kill("SIGKILL");
      } catch { /* The process can exit before the signal. */ }
    }
  };
  async function collect(stream) {
    const chunks = [];
    for await (const chunk of stream) {
      outputBytes += chunk.length;
      if (outputBytes > MAX_OUTPUT_BYTES) {
        stop();
        fail("Native preparation output exceeds its limit");
      }
      chunks.push(chunk);
    }
    const bytes = new Uint8Array(
      chunks.reduce((sum, chunk) => sum + chunk.length, 0),
    );
    let offset = 0;
    for (const chunk of chunks) {
      bytes.set(chunk, offset);
      offset += chunk.length;
    }
    return bytes;
  }
  let limitError;
  let checking = false;
  let poll = Promise.resolve();
  const timer = setInterval(() => {
    if (checking) return;
    checking = true;
    poll = (async () => {
      try {
        remaining(until);
        await inventory(directory, false);
      } catch (error) {
        limitError = error;
        stop();
      } finally {
        checking = false;
      }
    })();
  }, 25);
  const expiry = setTimeout(stop, milliseconds);
  until.signal?.addEventListener("abort", stop, { once: true });
  if (until.signal?.aborted) stop();
  const processes = [
    child.status,
    collect(child.stdout),
    collect(child.stderr),
  ];
  try {
    const [status, stdout] = await Promise.all(processes);
    remaining(until);
    if (limitError) throw limitError;
    await inventory(directory, false);
    return { code: status.code, stdout };
  } finally {
    clearInterval(timer);
    clearTimeout(expiry);
    until.signal?.removeEventListener("abort", stop);
    stop();
    await Promise.allSettled(processes);
    await poll;
  }
}
async function discover(directory, source, language, until) {
  const scratch = await Deno.makeTempDir({
    dir: directory,
    prefix: "inspection-",
  });
  try {
    const path = `${scratch}/source.${
      language === "typescript" ? "ts" : "mjs"
    }`;
    const config = `${scratch}/deno.json`;
    await Deno.writeTextFile(path, source, { createNew: true });
    await Deno.writeTextFile(
      config,
      JSON.stringify({
        lint: {
          plugins: [import.meta.url],
          rules: { tags: [], include: [RULE_ID] },
        },
        nodeModulesDir: "none",
        lock: false,
      }),
      { createNew: true },
    );
    const output = await native(directory, [
      "lint",
      "--json",
      "--config=" + config,
      "--deny-import",
      path,
    ], until);
    let record;
    try {
      const result = JSON.parse(decoder.decode(output.stdout));
      if (
        output.code !== 1 || result.errors?.length !== 0 ||
        result.diagnostics?.length !== 1
      ) fail("Invalid native source inspection");
      const diagnostic = result.diagnostics[0];
      if (diagnostic.code !== RULE_ID) {
        fail("Incomplete native source inspection");
      }
      record = JSON.parse(diagnostic.message);
    } catch {
      fail("Source requires complete native JavaScript inspection");
    }
    if (
      !exactFields(record, ["revision", "rejected", "requirements"]) ||
      record.revision !== 1 ||
      record.rejected !== false || !Array.isArray(record.requirements) ||
      record.requirements.length > MAX_REQUIREMENTS ||
      record.requirements.some((value) => !packageReference(value))
    ) {
      fail(
        "Use literal npm or JSR imports without dynamic imports or lint directives",
      );
    }
    return record.requirements;
  } finally {
    await Deno.remove(scratch, { recursive: true });
  }
}
function moduleSource(requirements) {
  return requirements.map((value) => `import ${JSON.stringify(value)};`).join(
    "\n",
  ) + "\n";
}
function validateLock(lock) {
  if (
    !lock || lock.version !== "5" ||
    Object.keys(lock).some((key) =>
      !["version", "specifiers", "npm", "jsr", "remote", "redirects"].includes(
        key,
      )
    ) ||
    Object.keys(lock.specifiers ?? {}).some((value) =>
      !packageReference(value)
    ) ||
    Object.keys(lock.remote ?? {}).some((value) =>
      !match(value, /^https:\/\/jsr\.io\/[A-Za-z0-9_@./+-]+$/)
    ) ||
    Object.keys(lock.redirects ?? {}).length !== 0
  ) fail("Native package lock contains unsupported registry content");
}
async function cachedGraph(directory, until) {
  // --no-run caches this generated import-only module. No user module is evaluated.
  const output = await native(directory, [
    "test",
    "--no-run",
    "--no-check",
    "--no-config",
    "--no-prompt",
    "--cached-only",
    "--frozen",
    "--node-modules-dir=none",
    "--allow-import=jsr.io,registry.npmjs.org",
    "--deny-net",
    "--deny-run",
    "--lock=" + `${directory}/${LOCK_NAME}`,
    `${directory}/${MODULE_NAME}`,
  ], until);
  if (output.code !== 0) {
    fail("Native frozen package graph is unavailable in its private cache");
  }
}
async function removeTransientCache(directory) {
  for await (const entry of Deno.readDir(`${directory}/deno-cache`)) {
    if (!["npm", "remote"].includes(entry.name)) {
      await Deno.remove(`${directory}/deno-cache/${entry.name}`, {
        recursive: true,
      });
    }
  }
}
function contentPath(name) {
  return [LOCK_NAME, MODULE_NAME].includes(name) ||
    name.startsWith("deno-cache/npm/registry.npmjs.org/") ||
    match(name, /^deno-cache\/remote\/https\/jsr\.io\/[a-f0-9]{64}$/);
}
async function privateDirectory(directory) {
  const stat = await Deno.lstat(directory);
  if (!stat.isDirectory || stat.mode === null || (stat.mode & 0o077) !== 0) {
    fail("Preparation requires a private directory");
  }
}
async function withCapacity(operation) {
  if (Deno.version.deno !== RUNTIME.slice(5)) {
    fail("Preparation requires the pinned Deno runtime");
  }
  if (activeJobs >= 2) fail("JavaScript preparation capacity is full");
  activeJobs++;
  try {
    return await operation();
  } finally {
    activeJobs--;
  }
}

export async function prepareJavaScriptCodePackages(
  directory,
  source,
  language = "javascript",
  options = {},
) {
  return await withCapacity(async () => {
    const until = deadline(options);
    remaining(until);
    if (
      !["javascript", "typescript"].includes(language) ||
      typeof source !== "string" ||
      source.trim().length === 0 ||
      encoder.encode(source).length > MAX_SOURCE_BYTES ||
      !source.isWellFormed()
    ) fail("JavaScript preparation source is invalid");
    await privateDirectory(directory);
    for await (const _entry of Deno.readDir(directory)) {
      fail(
        "Preparation requires empty staging; verify a resolved bundle instead",
      );
    }
    const requirements = await discover(directory, source, language, until);
    await Deno.writeTextFile(
      `${directory}/${MODULE_NAME}`,
      moduleSource(requirements),
      { createNew: true },
    );
    // Seed an empty native lock because Deno omits a new lock for an empty graph.
    await Deno.writeTextFile(`${directory}/${LOCK_NAME}`, '{"version":"5"}\n', {
      createNew: true,
    });
    const output = await native(directory, [
      "cache",
      "--no-config",
      "--no-check",
      "--node-modules-dir=none",
      "--frozen=false",
      "--allow-import=jsr.io,registry.npmjs.org",
      "--lock=" + `${directory}/${LOCK_NAME}`,
      `${directory}/${MODULE_NAME}`,
    ], until);
    if (output.code !== 0) fail("Native dependency preparation failed");
    validateLock(
      JSON.parse(
        decoder.decode(
          await readRegular(`${directory}/${LOCK_NAME}`, 1024 * 1024),
        ),
      ),
    );
    await cachedGraph(directory, until);
    await removeTransientCache(directory);
    const content = {
      revision: 1,
      runtime: RUNTIME,
      requirements,
      files: await inventory(directory),
    };
    if (content.files.some((file) => !contentPath(file.name))) {
      fail("Native package cache contains unsupported registry content");
    }
    const bundle = {
      ...content,
      digest: await digest(encoder.encode(JSON.stringify(content))),
    };
    const bytes = encoder.encode(JSON.stringify(bundle));
    if (bytes.length > MAX_METADATA_BYTES) {
      fail("Package bundle metadata exceeds its limit");
    }
    remaining(until);
    await Deno.writeFile(`${directory}/${BUNDLE_NAME}`, bytes, {
      createNew: true,
      mode: 0o600,
    });
    remaining(until);
    return bundle;
  });
}

// Reuse verifies all recorded bytes. It cannot refresh or resolve missing content.
export async function verifyJavaScriptCodePackages(
  directory,
  expectedDigest,
  options = {},
) {
  return await withCapacity(async () => {
    const until = deadline(options);
    remaining(until);
    await privateDirectory(directory);
    if (!validDigest(expectedDigest)) {
      fail("Reuse requires a recorded bundle digest");
    }
    const bytes = await readRegular(
      `${directory}/${BUNDLE_NAME}`,
      MAX_METADATA_BYTES,
    );
    const bundle = JSON.parse(decoder.decode(bytes));
    if (
      !exactFields(bundle, [
        "revision",
        "runtime",
        "requirements",
        "files",
        "digest",
      ]) ||
      bundle.revision !== 1 || bundle.runtime !== RUNTIME ||
      bundle.digest !== expectedDigest ||
      !Array.isArray(bundle.requirements) ||
      bundle.requirements.length > MAX_REQUIREMENTS ||
      bundle.requirements.some((value, index) =>
        !packageReference(value) ||
        index > 0 && value <= bundle.requirements[index - 1]
      ) ||
      !Array.isArray(bundle.files) || bundle.files.length < 2 ||
      bundle.files.length > MAX_FILES
    ) fail("Recorded JavaScript bundle is invalid");
    let total = 0;
    for (const [index, file] of bundle.files.entries()) {
      if (
        !exactFields(file, ["name", "bytes", "sha256"]) ||
        !safePath(file.name) ||
        !contentPath(file.name) ||
        index > 0 && file.name <= bundle.files[index - 1].name ||
        !Number.isSafeInteger(file.bytes) ||
        file.bytes < 0 || file.bytes > MAX_FILE_BYTES ||
        !validDigest(file.sha256)
      ) fail("Recorded JavaScript file is invalid");
      total += file.bytes;
    }
    const content = {
      revision: bundle.revision,
      runtime: bundle.runtime,
      requirements: bundle.requirements,
      files: bundle.files,
    };
    if (
      total > MAX_CACHE_BYTES ||
      await digest(encoder.encode(JSON.stringify(content))) !==
        expectedDigest ||
      JSON.stringify(bundle) !== decoder.decode(bytes)
    ) fail("Recorded JavaScript bundle digest differs");
    const actual = await inventory(directory);
    if (JSON.stringify(actual) !== JSON.stringify(bundle.files)) {
      fail("Recorded JavaScript content differs");
    }
    if (
      decoder.decode(
        await readRegular(`${directory}/${MODULE_NAME}`, 40 * 1024),
      ) !== moduleSource(bundle.requirements)
    ) fail("Recorded JavaScript imports differ");
    validateLock(
      JSON.parse(
        decoder.decode(
          await readRegular(`${directory}/${LOCK_NAME}`, 1024 * 1024),
        ),
      ),
    );
    // Inspect a disposable copy because Deno can update analysis caches.
    const scratch = await Deno.makeTempDir({
      prefix: "elitea-javascript-verify-",
      ...(options.scratchRoot ? { dir: options.scratchRoot } : {}),
    });
    try {
      for (const file of bundle.files) {
        const target = `${scratch}/${file.name}`;
        const bytes = await readRegular(
          `${directory}/${file.name}`,
          MAX_FILE_BYTES,
        );
        if (
          bytes.length !== file.bytes || await digest(bytes) !== file.sha256
        ) fail("Recorded JavaScript content changed during staging");
        remaining(until);
        await Deno.mkdir(target.slice(0, target.lastIndexOf("/")), {
          recursive: true,
          mode: 0o700,
        });
        await Deno.writeFile(target, bytes, { createNew: true });
      }
      await cachedGraph(scratch, until);
    } finally {
      await Deno.remove(scratch, { recursive: true });
    }
    remaining(until);
    return bundle;
  });
}
