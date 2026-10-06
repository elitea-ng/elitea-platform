import {preparedCapabilities} from "./platform_prepared.mjs";
// Image-owned native identity. Callers supply identities, never commands or paths.
const encoder = new TextEncoder();
export const METADATA = "elitea-native-bundle-v2.json";
export const READY = "elitea-native-ready-v2.json";
export function fail() {
  throw new Error("Native dependencies differ from the admitted request");
}
export function canonical(value) {
  if (Array.isArray(value)) return `[${value.map(canonical).join(",")}]`;
  if (value !== null && typeof value === "object") {
    return `{${
      Object.keys(value).sort().map((k) =>
        `${JSON.stringify(k)}:${canonical(value[k])}`
      ).join(",")
    }}`;
  }
  return JSON.stringify(value);
}
export async function sha(bytes) {
  return [...new Uint8Array(await crypto.subtle.digest("SHA-256", bytes))].map(
    (b) => b.toString(16).padStart(2, "0"),
  ).join("");
}
export function fields(v, n) {
  return v && typeof v === "object" && !Array.isArray(v) &&
    Object.keys(v).length === n.length && n.every((k) => Object.hasOwn(v, k));
}
export const digest = (v) =>
  typeof v === "string" && /^[a-f0-9]{64}$/.exec(v)?.[0] === v;
export function platform(v) {
  return fields(v, ["os", "arch", "abi"]) && v.os === "linux" &&
    ["amd64", "arm64"].includes(v.arch) && v.abi === "gnu";
}
export function actualPlatform(v) {
  return platform(v) && Deno.build.os === "linux" &&
    v.arch ===
      (Deno.build.arch === "x86_64"
        ? "amd64"
        : Deno.build.arch === "aarch64"
        ? "arm64"
        : "");
}
export async function readRegular(path, maximum) {
  const before = await Deno.lstat(path);
  if (!before.isFile || before.size > maximum) fail();
  using f = await Deno.open(path, { read: true });
  const opened = await f.stat();
  if (
    !opened.isFile || opened.ino !== before.ino || opened.size !== before.size
  ) fail();
  const out = new Uint8Array(before.size);
  let pos = 0;
  while (pos < out.length) {
    const n = await f.read(out.subarray(pos));
    if (!n) fail();
    pos += n;
  }
  if (await f.read(new Uint8Array(1)) !== null) fail();
  return out;
}
export async function writeImmutable(path, bytes) {
  const compare = async () => {
    const old = await readRegular(path, bytes.length);
    if (old.length !== bytes.length || await sha(old) !== await sha(bytes)) {
      fail();
    }
  };
  try {
    await compare();
    return;
  } catch (e) {
    if (!(e instanceof Deno.errors.NotFound)) throw e;
  }
  const temporary = await Deno.makeTempFile({
    dir: path.slice(0, path.lastIndexOf("/")),
    prefix: ".native-publish-",
  });
  try {
    using file = await Deno.open(temporary, { write: true, truncate: true });
    let p = 0;
    while (p < bytes.length) {
      const n = await file.write(bytes.subarray(p));
      if (!n) fail();
      p += n;
    }
    await file.sync();
    try {
      await Deno.link(temporary, path);
    } catch (e) {
      if (!(e instanceof Deno.errors.AlreadyExists)) throw e;
      await compare();
    }
  } finally {
    await Deno.remove(temporary);
  }
}
export async function preparationFingerprint(job) {
  const bytes = encoder.encode(JSON.stringify(job));
  const prefix = encoder.encode("elitea.sandbox.preparation-job.v1\0");
  const size = new Uint8Array(8);
  new DataView(size.buffer).setBigUint64(0, BigInt(bytes.length));
  const all = new Uint8Array(prefix.length + 8 + bytes.length);
  all.set(prefix);
  all.set(size, prefix.length);
  all.set(bytes, prefix.length + 8);
  return await sha(all);
}
export function decodeJob(raw) {
  if (raw.length > 1024 * 1024) fail();
  const job = JSON.parse(new TextDecoder("utf-8", { fatal: true }).decode(raw));
  if (
    !fields(job, [
      "revision",
      "language",
      "source",
      "preparer_image_digest",
      "policy_revision",
      "timeout_seconds",
      "platform",
      "execution_image_digest",
      "execution_policy_revision",
    ]) || job.revision !== 2 ||
    !["javascript", "typescript"].includes(job.language) ||
    !platform(job.platform) || typeof job.source !== "string" ||
    !job.source.trim() || !job.source.isWellFormed() ||
    job.source.includes("\0") ||
    encoder.encode(job.source).length > 256 * 1024 ||
    ![job.preparer_image_digest, job.execution_image_digest].every((v) =>
      typeof v === "string" && v.startsWith("sha256:") && digest(v.slice(7))
    ) ||
    ![job.policy_revision, job.execution_policy_revision].every((v) =>
      typeof v === "string" && /^[A-Za-z0-9._-]{1,128}$/.exec(v)?.[0] === v
    ) || !Number.isInteger(job.timeout_seconds) || job.timeout_seconds < 1 ||
    job.timeout_seconds > 120
  ) fail();
  // Exact typed Rust serialization is the authenticated request; no duplicate or null fields.
  const ordered = {
    revision: job.revision,
    language: job.language,
    source: job.source,
    preparer_image_digest: job.preparer_image_digest,
    policy_revision: job.policy_revision,
    timeout_seconds: job.timeout_seconds,
    platform: {
      os: job.platform.os,
      arch: job.platform.arch,
      abi: job.platform.abi,
    },
    execution_image_digest: job.execution_image_digest,
    execution_policy_revision: job.execution_policy_revision,
  };
  if (JSON.stringify(ordered) !== new TextDecoder().decode(raw)) fail();
  return ordered;
}
export async function createEnvelope(job, payload) {
  const content = {
    revision: 2,
    kind: "deno",
    language: job.language,
    platform: job.platform,
    preparation_sha256: await preparationFingerprint(job),
    source_sha256: await sha(encoder.encode(job.source)),
    execution_image_digest: job.execution_image_digest,
    execution_policy_revision: job.execution_policy_revision,
    payload,
  };
  if (encoder.encode(JSON.stringify(payload)).length > 126 * 1024) fail();
  const record = {
    ...content,
    digest: await sha(encoder.encode(canonical(content))),
  };
  if (encoder.encode(canonical(record)).length > 128 * 1024) fail();
  return record;
}
export async function validateEnvelope(raw, expectedRoot) {
  if (raw.length > 128 * 1024 || !digest(expectedRoot)) fail();
  const text = new TextDecoder("utf-8", { fatal: true }).decode(raw);
  const r = JSON.parse(text);
  if (
    !fields(r, [
      "revision",
      "kind",
      "language",
      "platform",
      "preparation_sha256",
      "source_sha256",
      "execution_image_digest",
      "execution_policy_revision",
      "payload",
      "digest",
    ]) || r.revision !== 2 || r.kind !== "deno" ||
    !["javascript", "typescript"].includes(r.language) ||
    !platform(r.platform) || !digest(r.preparation_sha256) ||
    !digest(r.source_sha256) || r.digest !== expectedRoot ||
    typeof r.execution_image_digest !== "string" ||
    !r.execution_image_digest.startsWith("sha256:") ||
    !digest(r.execution_image_digest.slice(7)) ||
    typeof r.execution_policy_revision !== "string" ||
    /^[A-Za-z0-9._-]{1,128}$/.exec(r.execution_policy_revision)?.[0] !==
      r.execution_policy_revision ||
    canonical(r) !== text
  ) fail();
  const { digest: _root, ...content } = r;
  if (await sha(encoder.encode(canonical(content))) !== expectedRoot) fail();
  return r;
}
export async function bindExecution(request, record) {
  const n = request.native_dependencies;
  if (
    preparedCapabilities(request).baseRevision !== 3 ||
    request.dependency_bundle_sha256 !== record.digest ||
    !fields(n, ["kind", "platform", "preparation_sha256", "source_sha256"]) ||
    n.kind !== record.kind || request.language !== record.language ||
    canonical(n.platform) !== canonical(record.platform) ||
    n.preparation_sha256 !== record.preparation_sha256 ||
    n.source_sha256 !== record.source_sha256 ||
    request.image_digest !== record.execution_image_digest ||
    request.policy_revision !== record.execution_policy_revision ||
    typeof request.source !== "string" ||
    await sha(encoder.encode(request.source)) !== record.source_sha256
  ) fail();
}
// v1 inner record retains its original ordered digest; outer metadata uses sorted JSON.
export function orderedPayload(p) {
  return {
    revision: p.revision,
    runtime: p.runtime,
    requirements: p.requirements,
    files: p.files?.map((f) => ({
      name: f.name,
      bytes: f.bytes,
      sha256: f.sha256,
    })),
    digest: p.digest,
  };
}
