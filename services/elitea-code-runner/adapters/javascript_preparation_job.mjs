// Trusted holding preparer. User source is inspected and never evaluated.
import {
  actualPlatform,
  canonical,
  createEnvelope,
  decodeJob,
  fail,
  orderedPayload,
  readRegular,
  sha,
  writeImmutable,
} from "./native_bundle.mjs";
import {
  prepareJavaScriptCodePackages,
  verifyJavaScriptCodePackages,
} from "./prepare_javascript_code.mjs";
const encoder = new TextEncoder();
export async function runJavaScriptPreparation({
  workspace = "/workspace",
  prepare = prepareJavaScriptCodePackages,
  verify = verifyJavaScriptCodePackages,
  platformCheck = actualPlatform,
  sleep = (ms) => new Promise((r) => setTimeout(r, ms)),
  now = () => Date.now(),
  mono = () => performance.now(),
} = {}) {
  const start = now(), started = mono();
  const job = decodeJob(
    await readRegular(`${workspace}/.elitea-code.json`, 1024 * 1024),
  );
  if (!platformCheck(job.platform) || Deno.version.deno !== "2.5.4") fail();
  let deadline = started + job.timeout_seconds * 1000;
  const remaining = () => {
    const n = deadline - mono();
    if (n <= 0) throw new Error("deadline_exceeded");
    return n;
  };
  const dir = `${workspace}/native-dependencies`,
    cache = `${dir}/cache`,
    markerPath = `${workspace}/.elitea-native-preparation.json`;
  let marker = null;
  try {
    marker = JSON.parse(
      new TextDecoder().decode(await readRegular(markerPath, 256 * 1024)),
    );
  } catch (e) {
    if (!(e instanceof Deno.errors.NotFound)) throw e;
  }
  const options = () => ({
    timeoutSeconds: Math.max(1, Math.min(120, Math.ceil(remaining() / 1000))),
    scratchRoot: workspace,
  });
  if (marker) {
    if (
      marker.revision !== 2 || marker.status !== "resolved" ||
      marker.source_sha256 !== await sha(encoder.encode(job.source)) ||
      marker.preparer_image_digest !== job.preparer_image_digest ||
      marker.policy_revision !== job.policy_revision ||
      marker.timeout_seconds !== job.timeout_seconds ||
      !Number.isSafeInteger(marker.started_unix_ms) ||
      marker.started_unix_ms > start || marker.started_unix_ms <= 0 ||
      marker.deadline_unix_ms - marker.started_unix_ms !==
        job.timeout_seconds * 1000
    ) fail();
    deadline = Math.min(deadline, started + marker.deadline_unix_ms - start);
    remaining();
    const verified = await verify(
      cache,
      marker.bundle.payload.digest,
      options(),
    );
    const expected = await createEnvelope(job, verified);
    if (canonical(expected) !== canonical(marker.bundle)) fail();
  } else {
    await Deno.mkdir(dir, { mode: 0o700 });
    await Deno.mkdir(cache, { mode: 0o700 });
    await Deno.mkdir(`${dir}/objects`, { mode: 0o700 });
    const payload = await prepare(cache, job.source, job.language, options());
    const verified = await verify(cache, payload.digest, options());
    if (canonical(verified) !== canonical(payload)) fail();
    const bundle = await createEnvelope(job, orderedPayload(verified));
    for (const f of verified.files) {
      remaining();
      const raw = await readRegular(`${cache}/${f.name}`, 32 * 1024 * 1024);
      if (raw.length !== f.bytes || await sha(raw) !== f.sha256) fail();
      await writeImmutable(`${dir}/objects/${f.sha256}.blob`, raw);
    }
    marker = {
      revision: 2,
      status: "resolved",
      source_sha256: bundle.source_sha256,
      preparer_image_digest: job.preparer_image_digest,
      policy_revision: job.policy_revision,
      timeout_seconds: job.timeout_seconds,
      started_unix_ms: start,
      deadline_unix_ms: start + job.timeout_seconds * 1000,
      bundle,
    };
    remaining();
    await writeImmutable(markerPath, encoder.encode(canonical(marker)));
  }
  while (true) {
    const ms = remaining();
    try {
      const release = await Deno.lstat(
        `${workspace}/.elitea-native-preparation-release`,
      );
      if (!release.isFile || release.size !== 0) fail();
      return marker;
    } catch (e) {
      if (!(e instanceof Deno.errors.NotFound)) throw e;
    }
    await sleep(Math.min(100, ms));
  }
}
if (import.meta.main) {
  try {
    if (Deno.args.length) fail();
    for (
      const method of ["log", "info", "warn", "error", "debug"]
    ) console[method] = () => {};
    await runJavaScriptPreparation();
  } catch {
    await Deno.stderr.write(
      encoder.encode(
        '{"revision":2,"status":"failed","error":"native_preparation_failed"}\n',
      ),
    );
    Deno.exit(1);
  }
}
