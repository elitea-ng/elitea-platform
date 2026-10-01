// Image-owned preparation adapter. Never execute the supplied Python source.
const encoder = new TextEncoder();
const decoder = new TextDecoder("utf-8", { fatal: true, ignoreBOM: true });
const MAX_JOB_BYTES = 1024 * 1024;
const MAX_MARKER_BYTES = 256 * 1024;
const SOURCE_BYTES = 256 * 1024;
// Match Rust str::trim() whitespace. JSON transport excludes unpaired surrogates.
const nonRustWhitespace =
  // deno-lint-ignore no-control-regex
  /[^\u0009-\u000d\u0020\u0085\u00a0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000]/;
const JOB_FIELDS = [
  "revision",
  "language",
  "source",
  "preparer_image_digest",
  "policy_revision",
  "timeout_seconds",
];
const MARKER_FIELDS = [
  "revision",
  "status",
  "source_sha256",
  "preparer_image_digest",
  "policy_revision",
  "timeout_seconds",
  "started_unix_ms",
  "deadline_unix_ms",
  "bundle",
];
const realClock = {
  wallNow: () => Date.now(),
  monotonicNow: () => performance.now(),
  setTimer: (callback, milliseconds) => setTimeout(callback, milliseconds),
  clearTimer: (timer) => clearTimeout(timer),
};

class PreparationError extends Error {
  constructor(code) {
    super(code);
    this.code = code;
  }
}
function fail(code = "invalid_preparation") {
  throw new PreparationError(code);
}
function exactFields(value, fields) {
  return value !== null && typeof value === "object" && !Array.isArray(value) &&
    Object.keys(value).length === fields.length &&
    fields.every((key) => Object.hasOwn(value, key));
}
function digestShape(value) {
  return typeof value === "string" && value.length === 64 &&
    /^[a-f0-9]{64}$/.test(value);
}
function fullMatch(value, pattern) {
  return typeof value === "string" && pattern.exec(value)?.[0] === value;
}
async function digest(bytes) {
  return Array.from(
    new Uint8Array(await crypto.subtle.digest("SHA-256", bytes)),
  )
    .map((byte) => byte.toString(16).padStart(2, "0")).join("");
}

// All job values are scalar. Scan field names before JSON.parse loses duplicates.
export function decodePreparationJob(bytes) {
  if (!(bytes instanceof Uint8Array) || bytes.length > MAX_JOB_BYTES) fail();
  let text;
  let job;
  try {
    text = decoder.decode(bytes);
    job = JSON.parse(text);
  } catch {
    fail();
  }
  if (!exactFields(job, JOB_FIELDS)) fail();
  // JSON strings cannot contain raw controls.
  const token =
    // deno-lint-ignore no-control-regex
    /"(?:[^"\\\u0000-\u001f]|\\(?:["\\/bfnrt]|u[0-9a-fA-F]{4}))*"|-?(?:0|[1-9]\d*)(?:\.\d+)?(?:[eE][+-]?\d+)?/y;
  const seen = new Set();
  let offset = 0;
  const whitespace = () => {
    while (/[ \t\r\n]/.test(text[offset] ?? "x")) offset++;
  };
  const take = () => {
    whitespace();
    token.lastIndex = offset;
    const match = token.exec(text);
    if (!match) fail();
    offset = token.lastIndex;
    return { value: JSON.parse(match[0]), raw: match[0] };
  };
  whitespace();
  if (text[offset++] !== "{") fail();
  for (let index = 0; index < JOB_FIELDS.length; index++) {
    const { value: key } = take();
    if (typeof key !== "string" || seen.has(key)) fail();
    seen.add(key);
    whitespace();
    if (text[offset++] !== ":") fail();
    const scalar = take();
    if (
      ["revision", "timeout_seconds"].includes(key) &&
      !fullMatch(scalar.raw, /^(?:0|[1-9][0-9]*)$/)
    ) fail();
    whitespace();
    if (text[offset++] !== (index === JOB_FIELDS.length - 1 ? "}" : ",")) {
      fail();
    }
  }
  whitespace();
  if (
    offset !== text.length || job.revision !== 1 || job.language !== "python" ||
    typeof job.source !== "string" ||
    !nonRustWhitespace.test(job.source) ||
    job.source.includes("\0") || !job.source.isWellFormed() ||
    encoder.encode(job.source).length > SOURCE_BYTES ||
    typeof job.preparer_image_digest !== "string" ||
    !fullMatch(job.preparer_image_digest, /^sha256:[a-f0-9]{64}$/) ||
    typeof job.policy_revision !== "string" ||
    !fullMatch(job.policy_revision, /^[A-Za-z0-9._-]{1,128}$/) ||
    !Number.isInteger(job.timeout_seconds) || job.timeout_seconds < 1 ||
    job.timeout_seconds > 3600
  ) fail();
  return job;
}

async function statIfPresent(path) {
  try {
    return await Deno.lstat(path);
  } catch (error) {
    if (error instanceof Deno.errors.NotFound) return null;
    throw error;
  }
}
async function readRegular(path, maximum) {
  const stat = await Deno.lstat(path);
  if (!stat.isFile || stat.size > maximum) fail("invalid_preparation_file");
  using file = await Deno.open(path, { read: true });
  const opened = await file.stat();
  if (!opened.isFile || opened.ino !== stat.ino || opened.size !== stat.size) {
    fail("invalid_preparation_file");
  }
  const bytes = new Uint8Array(stat.size);
  let offset = 0;
  while (offset < bytes.length) {
    const count = await file.read(bytes.subarray(offset));
    if (count === null || count === 0) fail("invalid_preparation_file");
    offset += count;
  }
  if (await file.read(new Uint8Array(1)) !== null) {
    fail("invalid_preparation_file");
  }
  return bytes;
}
function bundleShape(bundle) {
  if (
    !exactFields(bundle, [
      "revision",
      "runtime",
      "requirements",
      "files",
      "digest",
    ]) ||
    bundle.revision !== 1 || bundle.runtime !== "pyodide-0.29.0" ||
    !digestShape(bundle.digest) || !Array.isArray(bundle.requirements) ||
    bundle.requirements.length > 128 ||
    bundle.requirements.some((value) =>
      typeof value !== "string" || value.length === 0 || value.length > 256 ||
      !fullMatch(value, /^[\x20-\x7e]+$/) || /[:/@;]/.test(value)
    ) || !Array.isArray(bundle.files) || bundle.files.length === 0 ||
    bundle.files.length > 257
  ) fail("invalid_bundle");
  const names = new Set();
  let total = 0;
  for (const file of bundle.files) {
    if (
      !exactFields(file, ["name", "bytes", "sha256"]) ||
      typeof file.name !== "string" || file.name.length > 256 ||
      !fullMatch(file.name, /^[A-Za-z0-9_.+-]+\.(whl|json)$/) ||
      names.has(file.name) || !Number.isSafeInteger(file.bytes) ||
      file.bytes < 0 ||
      file.bytes > 32 * 1024 * 1024 || !digestShape(file.sha256)
    ) fail("invalid_bundle");
    names.add(file.name);
    total += file.bytes;
  }
  if (!names.has("elitea-python-lock.json") || total > 128 * 1024 * 1024) {
    fail("invalid_bundle");
  }
}

function remaining(clock, deadline) {
  const milliseconds = deadline - clock.monotonicNow();
  if (milliseconds <= 0) fail("deadline_exceeded");
  return milliseconds;
}
async function beforeDeadline(operation, clock, deadline) {
  const milliseconds = remaining(clock, deadline);
  let timer;
  const timeout = new Promise((_, reject) => {
    timer = clock.setTimer(
      () => reject(new PreparationError("deadline_exceeded")),
      milliseconds,
    );
  });
  try {
    const result = await Promise.race([operation(), timeout]);
    remaining(clock, deadline);
    return result;
  } finally {
    clock.clearTimer(timer);
  }
}

async function publishMarker(path, marker, clock, deadline) {
  const bytes = encoder.encode(JSON.stringify(marker));
  if (bytes.length > MAX_MARKER_BYTES) fail("invalid_bundle");
  const temporary = `${path}.tmp`;
  using file = await Deno.open(temporary, {
    write: true,
    createNew: true,
    mode: 0o600,
  });
  let offset = 0;
  while (offset < bytes.length) {
    const count = await file.write(bytes.subarray(offset));
    if (count <= 0) fail("preparation_failed");
    offset += count;
  }
  await file.sync();
  remaining(clock, deadline);
  await Deno.rename(temporary, path);
}

// Test injection changes paths and native functions. The CLI exposes no options.
export async function runPythonPreparation({
  workspace = "/workspace",
  prepare,
  verify,
  clock = realClock,
  sleep = (milliseconds) =>
    new Promise((resolve) => setTimeout(resolve, milliseconds)),
} = {}) {
  try {
    const started = clock.wallNow();
    const monotonicStarted = clock.monotonicNow();
    if (!(await Deno.lstat(workspace)).isDirectory) {
      fail("invalid_preparation_file");
    }
    const job = decodePreparationJob(
      await readRegular(`${workspace}/.elitea-code.json`, MAX_JOB_BYTES),
    );
    let deadline = monotonicStarted + job.timeout_seconds * 1000;
    remaining(clock, deadline);
    if (typeof prepare !== "function" || typeof verify !== "function") fail();
    const directory = `${workspace}/python-dependencies`;
    const markerPath = `${workspace}/.elitea-python-preparation.json`;
    const releasePath = `${workspace}/.elitea-python-preparation-release`;
    const sourceDigest = await digest(encoder.encode(job.source));
    const directoryStat = await statIfPresent(directory);
    const markerStat = await statIfPresent(markerPath);
    const temporaryStat = await statIfPresent(`${markerPath}.tmp`);
    let marker;
    if (markerStat) {
      const text = decoder.decode(
        await readRegular(markerPath, MAX_MARKER_BYTES),
      );
      marker = JSON.parse(text);
      if (
        !directoryStat?.isDirectory ||
        (directoryStat.mode !== null && (directoryStat.mode & 0o077) !== 0) ||
        temporaryStat ||
        !exactFields(marker, MARKER_FIELDS) ||
        JSON.stringify(marker) !== text ||
        marker.revision !== 1 || marker.status !== "resolved" ||
        marker.source_sha256 !== sourceDigest ||
        marker.preparer_image_digest !== job.preparer_image_digest ||
        marker.policy_revision !== job.policy_revision ||
        marker.timeout_seconds !== job.timeout_seconds ||
        !Number.isSafeInteger(marker.started_unix_ms) ||
        marker.started_unix_ms <= 0 ||
        !Number.isSafeInteger(marker.deadline_unix_ms) ||
        marker.deadline_unix_ms - marker.started_unix_ms !==
          job.timeout_seconds * 1000 ||
        marker.started_unix_ms > started
      ) fail("invalid_reuse");
      deadline = Math.min(
        deadline,
        monotonicStarted + marker.deadline_unix_ms - started,
      );
      remaining(clock, deadline);
      bundleShape(marker.bundle);
      const verified = await beforeDeadline(
        () => verify(directory, marker.bundle.digest),
        clock,
        deadline,
      );
      bundleShape(verified);
      if (JSON.stringify(verified) !== JSON.stringify(marker.bundle)) {
        fail("invalid_reuse");
      }
    } else {
      if (directoryStat || temporaryStat || await statIfPresent(releasePath)) {
        fail("invalid_reuse");
      }
      await Deno.mkdir(directory, { mode: 0o700 });
      const bundle = await beforeDeadline(
        () => prepare(directory, job.source),
        clock,
        deadline,
      );
      bundleShape(bundle);
      const verified = await beforeDeadline(
        () => verify(directory, bundle.digest),
        clock,
        deadline,
      );
      bundleShape(verified);
      if (JSON.stringify(verified) !== JSON.stringify(bundle)) {
        fail("invalid_bundle");
      }
      marker = {
        revision: 1,
        status: "resolved",
        source_sha256: sourceDigest,
        preparer_image_digest: job.preparer_image_digest,
        policy_revision: job.policy_revision,
        timeout_seconds: job.timeout_seconds,
        started_unix_ms: started,
        deadline_unix_ms: started + job.timeout_seconds * 1000,
        bundle: verified,
      };
      await publishMarker(markerPath, marker, clock, deadline);
    }
    while (true) {
      const milliseconds = remaining(clock, deadline);
      const release = await statIfPresent(releasePath);
      if (release) {
        if (!release.isFile || release.size !== 0) fail("invalid_release");
        remaining(clock, deadline);
        return marker;
      }
      await sleep(Math.min(100, milliseconds));
    }
  } catch (error) {
    if (error instanceof PreparationError) throw error;
    fail("preparation_failed");
  }
}

export function preparationFailure(error) {
  return {
    revision: 1,
    status: "failed",
    error: error instanceof PreparationError
      ? error.code
      : "preparation_failed",
  };
}

if (import.meta.main) {
  try {
    if (Deno.args.length !== 0) fail();
    // Native diagnostics can contain registry URLs or requirements. Retain metadata only in the marker.
    for (const method of ["log", "info", "warn", "error", "debug"]) {
      console[method] = () => {};
    }
    let native;
    const nativeModule =
      new URL("./prepare_python_code.mjs", import.meta.url).href;
    const loadNative = () => native ??= import(nativeModule);
    await runPythonPreparation({
      prepare: async (...args) =>
        (await loadNative()).preparePythonCodePackages(...args),
      verify: async (...args) =>
        (await loadNative()).verifyPythonCodePackages(...args),
    });
  } catch (error) {
    const failure = preparationFailure(error);
    await Deno.stderr.write(encoder.encode(JSON.stringify(failure) + "\n"));
    Deno.exit(failure.error === "deadline_exceeded" ? 124 : 1);
  }
}
