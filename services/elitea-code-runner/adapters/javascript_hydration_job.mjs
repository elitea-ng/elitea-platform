// Inert hydration uses fixed paths and authenticated digest aliases.
import {
  actualPlatform,
  bindExecution,
  canonical,
  fail,
  METADATA,
  orderedPayload,
  readRegular,
  READY,
  sha,
  validateEnvelope,
  writeImmutable,
} from "./native_bundle.mjs";
import { verifyJavaScriptCodePackages } from "./prepare_javascript_code.mjs";
const encoder = new TextEncoder();
export async function hydrateJavaScript(
  {
    workspace = "/workspace",
    verify = verifyJavaScriptCodePackages,
    platformCheck = actualPlatform,
    deadlineMonotonicMs = performance.now() + 25_000,
    signal,
    ownedPhase = false,
    clock = () => performance.now(),
  } = {},
) {
  const check = () => {
    if (signal?.aborted) {
      throw new DOMException("Hydration cancelled", "AbortError");
    }
    if (
      !Number.isFinite(deadlineMonotonicMs) ||
      clock() >= deadlineMonotonicMs
    ) fail();
  };
  const phaseRoot = `${workspace}/native-bundle` +
    (ownedPhase ? "/.elitea-native-finalizing" : "");
  const phaseDirectory = await Deno.lstat(phaseRoot);
  if (!phaseDirectory.isDirectory) fail();
  const verification = {
    scratchRoot: phaseRoot,
    timeoutSeconds: 27,
    deadlineMonotonicMs,
    signal,
  };
  check();
  const base = `${workspace}/native-bundle`;
  const request = JSON.parse(
    new TextDecoder().decode(
      await readRegular(`${workspace}/.elitea-code.json`, 1024 * 1024),
    ),
  );
  const raw = await readRegular(`${base}/${METADATA}`, 128 * 1024);
  const record = await validateEnvelope(raw, request.dependency_bundle_sha256);
  await bindExecution(request, record);
  if (!platformCheck(record.platform) || Deno.version.deno !== "2.5.4") fail();
  const payload = orderedPayload(record.payload);
  const cache = `${base}/cache`;
  let existing = false;
  try {
    const stat = await Deno.lstat(cache);
    if (!stat.isDirectory) fail();
    existing = true;
  } catch (e) {
    if (!(e instanceof Deno.errors.NotFound)) throw e;
  }
  if (!existing) {
    const scratch = await Deno.makeTempDir({
      dir: phaseRoot,
      prefix: "hydrate-",
    });
    try {
      if (
        !Array.isArray(payload.files) || payload.files.length < 2 ||
        payload.files.length > 512
      ) fail();
      let total = 0;
      for (const f of payload.files) {
        check();
        if (
          typeof f.name !== "string" || f.name.length > 256 ||
          /^[-A-Za-z0-9_@.+/]+$/.exec(f.name)?.[0] !== f.name ||
          f.name.split("/").some((p) => !p || p === "." || p === "..") ||
          f.name.split("/").length > 24 ||
          !Number.isSafeInteger(f.bytes) || f.bytes < 0 ||
          f.bytes > 32 * 1024 * 1024 || typeof f.sha256 !== "string" ||
          /^[a-f0-9]{64}$/.exec(f.sha256)?.[0] !== f.sha256
        ) fail();
        total += f.bytes;
        if (total > 128 * 1024 * 1024) fail();
        const bytes = await readRegular(
          `${base}/objects/${f.sha256}.blob`,
          f.bytes,
        );
        if (bytes.length !== f.bytes || await sha(bytes) !== f.sha256) fail();
        const target = `${scratch}/${f.name}`;
        await Deno.mkdir(target.slice(0, target.lastIndexOf("/")), {
          recursive: true,
          mode: 0o700,
        });
        await writeImmutable(target, bytes);
      }
      await writeImmutable(
        `${scratch}/elitea-javascript-bundle.json`,
        encoder.encode(JSON.stringify(payload)),
      );
      check();
      await verify(scratch, payload.digest, verification);
      check();
      try {
        await Deno.rename(scratch, cache);
      } catch (e) {
        if (
          !(e instanceof Deno.errors.AlreadyExists) ||
          !(await Deno.lstat(cache)).isDirectory
        ) throw e;
      }
    } finally {
      try {
        await Deno.remove(scratch, { recursive: true });
      } catch (e) {
        if (!(e instanceof Deno.errors.NotFound)) throw e;
      }
    }
  }
  check();
  const verified = await verify(cache, payload.digest, verification);
  check();
  if (canonical(verified) !== canonical(payload)) fail();
  await writeImmutable(`${base}/${READY}`, raw);
  return record;
}
// Execution verifies bytes without native subprocess permission; hydration did the frozen graph check.
export async function verifyJavaScriptExecution(workspace = "/workspace") {
  const base = `${workspace}/native-bundle`;
  const request = JSON.parse(
    new TextDecoder().decode(
      await readRegular(`${workspace}/.elitea-code.json`, 1024 * 1024),
    ),
  );
  const raw = await readRegular(`${base}/${READY}`, 128 * 1024);
  const record = await validateEnvelope(raw, request.dependency_bundle_sha256);
  await bindExecution(request, record);
  if (!actualPlatform(record.platform) || Deno.version.deno !== "2.5.4") fail();
  for (const f of record.payload.files) {
    const bytes = await readRegular(
      `${base}/cache/${f.name}`,
      32 * 1024 * 1024,
    );
    if (bytes.length !== f.bytes || await sha(bytes) !== f.sha256) fail();
  }
  return record;
}
if (import.meta.main) {
  try {
    if (Deno.args.length) fail();
    for (
      const method of ["log", "info", "warn", "error", "debug"]
    ) console[method] = () => {};
    const rawDeadline = Deno.env.get(
      "ELITEA_NATIVE_FINALIZATION_DEADLINE_UNIX_MS",
    );
    if (!/^[0-9]{1,16}$/.test(rawDeadline ?? "")) fail();
    const remainingMs = Number(rawDeadline) - Date.now();
    if (
      !Number.isSafeInteger(remainingMs) || remainingMs <= 0 ||
      remainingMs > 27_000
    ) fail();
    await hydrateJavaScript({
      deadlineMonotonicMs: performance.now() + remainingMs,
      ownedPhase: true,
    });
  } catch {
    await Deno.stderr.write(
      encoder.encode('{"error":"native_hydration_failed"}\n'),
    );
    Deno.exit(1);
  }
}
