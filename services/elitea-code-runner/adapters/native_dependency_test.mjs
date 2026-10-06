import {
  bindExecution,
  canonical,
  createEnvelope,
  decodeJob,
  METADATA,
  READY,
  sha,
  validateEnvelope,
} from "./native_bundle.mjs";
import { runJavaScriptPreparation } from "./javascript_preparation_job.mjs";
import { hydrateJavaScript } from "./javascript_hydration_job.mjs";
const encoder = new TextEncoder();
function assert(v) {
  if (!v) throw new Error("Assertion failed");
}
async function rejects(fn) {
  try {
    await fn();
  } catch {
    return;
  }
  throw new Error("Invalid identity was accepted");
}
function job() {
  return {
    revision: 2,
    language: "typescript",
    source:
      'throw new Error("user source executed during preparation"); export default 42;',
    preparer_image_digest: "sha256:" + "a".repeat(64),
    policy_revision: "js-preparation-v2",
    timeout_seconds: 30,
    platform: { os: "linux", arch: "arm64", abi: "gnu" },
    execution_image_digest: "sha256:" + "b".repeat(64),
    execution_policy_revision: "js-offline-v2",
  };
}
function execution(j, r) {
  return {
    revision: 3,
    language: j.language,
    source: j.source,
    input: {},
    image_digest: j.execution_image_digest,
    policy_revision: j.execution_policy_revision,
    timeout_seconds: 30,
    dependency_bundle_sha256: r.digest,
    native_dependencies: {
      kind: "deno",
      platform: j.platform,
      preparation_sha256: r.preparation_sha256,
      source_sha256: r.source_sha256,
    },
  };
}
Deno.test("native request decoder rejects duplicate, null, platform and revision downgrade", async () => {
  const j = job(), bytes = encoder.encode(JSON.stringify(j));
  assert(decodeJob(bytes).language === "typescript");
  for (
    const raw of [
      JSON.stringify(j).replace('"revision":2', '"revision":2,"revision":2'),
      JSON.stringify({ ...j, platform: null }),
      JSON.stringify({ ...j, revision: 1 }),
      JSON.stringify({ ...j, platform: { ...j.platform, arch: "host" } }),
    ]
  ) await rejects(() => decodeJob(encoder.encode(raw)));
});
Deno.test("native envelope binds source, image, platform and preparation independently", async () => {
  const j = job(),
    r = await createEnvelope(j, {
      revision: 1,
      runtime: "deno-2.5.4",
      requirements: [],
      files: [],
      digest: "a".repeat(64),
    });
  const raw = encoder.encode(canonical(r));
  assert((await validateEnvelope(raw, r.digest)).kind === "deno");
  await bindExecution(execution(j, r), r);
  for (
    const change of [
      { source: j.source + "\n" },
      { image_digest: j.preparer_image_digest },
      { revision: 2 },
      {
        native_dependencies: {
          ...execution(j, r).native_dependencies,
          platform: { ...j.platform, arch: "amd64" },
        },
      },
    ]
  ) await rejects(() => bindExecution({ ...execution(j, r), ...change }, r));
  await rejects(() =>
    validateEnvelope(
      encoder.encode(canonical({ ...r, language: "javascript" })),
      r.digest,
    )
  );
  await rejects(() =>
    validateEnvelope(
      encoder.encode('{"revision":2,' + canonical(r).slice(1)),
      r.digest,
    )
  );
});
Deno.test("holding native AST preparation, digest aliases, inert hydration, and replay stay offline", async () => {
  const workspace = await Deno.makeTempDir();
  try {
    const j = job();
    await Deno.writeTextFile(
      `${workspace}/.elitea-code.json`,
      JSON.stringify(j),
    );
    const marker = await runJavaScriptPreparation({
      workspace,
      platformCheck: () => true,
      sleep: async () => {
        await Deno.writeTextFile(
          `${workspace}/.elitea-native-preparation-release`,
          "",
          { createNew: true },
        );
      },
    });
    assert(marker.revision === 2 && marker.bundle.payload.files.length === 2);
    const emitted = await Deno.readTextFile(
      `${workspace}/.elitea-native-preparation.json`,
    );
    assert(emitted === canonical(marker));
    const embedded = /"bundle":(\{.*\}),"deadline_unix_ms":/.exec(emitted)?.[1];
    assert(embedded === canonical(marker.bundle));
    await validateEnvelope(encoder.encode(embedded), marker.bundle.digest);
    const r = marker.bundle, request = execution(j, r);
    await Deno.writeTextFile(
      `${workspace}/.elitea-code.json`,
      JSON.stringify(request),
    );
    await Deno.mkdir(`${workspace}/native-bundle`, { mode: 0o700 });
    await Deno.mkdir(`${workspace}/native-bundle/objects`, { mode: 0o700 });
    for (const f of r.payload.files) {
      await Deno.copyFile(
        `${workspace}/native-dependencies/objects/${f.sha256}.blob`,
        `${workspace}/native-bundle/objects/${f.sha256}.blob`,
      );
    }
    await Deno.writeTextFile(
      `${workspace}/native-bundle/${METADATA}`,
      canonical(r),
    );
    await Deno.mkdir(`${workspace}/native-bundle/.elitea-native-finalizing`, {
      mode: 0o700,
    });
    const hydrated = await hydrateJavaScript({
      workspace,
      platformCheck: () => true,
      ownedPhase: true,
    });
    assert(hydrated.digest === r.digest);
    assert(
      await Deno.readTextFile(`${workspace}/native-bundle/${READY}`) ===
        canonical(r),
    );
    await hydrateJavaScript({
      workspace,
      platformCheck: () => true,
      ownedPhase: true,
    });
    const remainingScratch = [];
    for await (
      const entry of Deno.readDir(
        `${workspace}/native-bundle/.elitea-native-finalizing`,
      )
    ) remainingScratch.push(entry.name);
    assert(remainingScratch.length === 0);
    const lock = r.payload.files.find((f) =>
      f.name === "elitea-javascript-lock.json"
    );
    await Deno.writeTextFile(
      `${workspace}/native-bundle/cache/${lock.name}`,
      "changed",
    );
    await rejects(() =>
      hydrateJavaScript({ workspace, platformCheck: () => true })
    );
  } finally {
    await Deno.remove(workspace, { recursive: true });
  }
});
Deno.test("hydration rejects traversal and changed object bytes before readiness", async () => {
  const workspace = await Deno.makeTempDir();
  try {
    const j = job();
    const payload = {
      revision: 1,
      runtime: "deno-2.5.4",
      requirements: [],
      files: [{
        name: "../escaped",
        bytes: 1,
        sha256: await sha(encoder.encode("a")),
      }, {
        name: "elitea-javascript-lock.json",
        bytes: 1,
        sha256: await sha(encoder.encode("a")),
      }],
      digest: "a".repeat(64),
    };
    const r = await createEnvelope(j, payload);
    await Deno.writeTextFile(
      `${workspace}/.elitea-code.json`,
      JSON.stringify(execution(j, r)),
    );
    await Deno.mkdir(`${workspace}/native-bundle/objects`, {
      recursive: true,
      mode: 0o700,
    });
    await Deno.writeTextFile(
      `${workspace}/native-bundle/${METADATA}`,
      canonical(r),
    );
    await rejects(() =>
      hydrateJavaScript({
        workspace,
        platformCheck: () => true,
        verify: () => {
          throw new Error("verify should not see unsafe path");
        },
      })
    );
    let ready = false;
    try {
      await Deno.lstat(`${workspace}/native-bundle/${READY}`);
      ready = true;
    } catch {}
    assert(!ready);
  } finally {
    await Deno.remove(workspace, { recursive: true });
  }
});

Deno.test("native Deno fixture matches Main and Rust canonical root and preparation fingerprint", async () => {
  const text = await Deno.readTextFile(
    new URL("./fixtures/native-deno-v2.json", import.meta.url),
  );
  const fixture = JSON.parse(text);
  const j = { ...job(), source: "export default 42;" };
  const actual = await createEnvelope(j, fixture.payload);
  assert(canonical(actual) === text);
  assert(
    actual.digest ===
      "e78a5f5d12fd32274106aa7d9c6584e7213634194aee5b1aff9b8bb8e66ea108",
  );
  assert(
    actual.preparation_sha256 ===
      "b43355889c53e4f8019d5997effa97027a82cd63d18f151faafc6294e99c5e1d",
  );
});

async function hydrationFixture(workspace) {
  const j = job();
  const contents = [
    ["elitea-javascript-dependencies.mjs", "\n"],
    ["elitea-javascript-lock.json", '{"version":"5"}\n'],
  ];
  const files = [];
  await Deno.mkdir(`${workspace}/native-bundle/objects`, {
    recursive: true,
    mode: 0o700,
  });
  for (const [name, text] of contents) {
    const bytes = encoder.encode(text), hash = await sha(bytes);
    files.push({ name, bytes: bytes.length, sha256: hash });
    await Deno.writeFile(
      `${workspace}/native-bundle/objects/${hash}.blob`,
      bytes,
      { createNew: true },
    );
  }
  const content = {
    revision: 1,
    runtime: "deno-2.5.4",
    requirements: [],
    files,
  };
  const payload = {
    ...content,
    digest: await sha(encoder.encode(JSON.stringify(content))),
  };
  const record = await createEnvelope(j, payload);
  await Deno.writeTextFile(
    `${workspace}/.elitea-code.json`,
    JSON.stringify(execution(j, record)),
  );
  await Deno.writeTextFile(
    `${workspace}/native-bundle/${METADATA}`,
    canonical(record),
  );
  return payload;
}
async function noReady(workspace) {
  try {
    await Deno.lstat(`${workspace}/native-bundle/${READY}`);
    throw new Error("Readiness appeared after cancellation or deadline expiry");
  } catch (error) {
    if (!(error instanceof Deno.errors.NotFound)) throw error;
  }
}
Deno.test("hydration staging and both verification passes share one phase deadline", async () => {
  const workspace = await Deno.makeTempDir();
  try {
    const payload = await hydrationFixture(workspace);
    const deadline = performance.now() + 1000;
    const seen = [];
    await hydrateJavaScript({
      workspace,
      platformCheck: () => true,
      deadlineMonotonicMs: deadline,
      verify: (_directory, _digest, options) => {
        seen.push(options.deadlineMonotonicMs);
        return payload;
      },
    });
    assert(seen.length === 2 && seen.every((value) => value === deadline));
  } finally {
    await Deno.remove(workspace, { recursive: true });
  }
});
Deno.test("hydration cannot restart the phase budget after the first verification expires", async () => {
  const workspace = await Deno.makeTempDir();
  try {
    const payload = await hydrationFixture(workspace);
    let calls = 0, now = 100;
    await rejects(() =>
      hydrateJavaScript({
        workspace,
        platformCheck: () => true,
        deadlineMonotonicMs: 200,
        clock: () => now,
        verify: () => {
          calls++;
          now = 201;
          return payload;
        },
      })
    );
    assert(calls === 1);
    await noReady(workspace);
  } finally {
    await Deno.remove(workspace, { recursive: true });
  }
});
Deno.test("hydration cancellation stops before the second pass and readiness", async () => {
  const workspace = await Deno.makeTempDir();
  try {
    const payload = await hydrationFixture(workspace),
      controller = new AbortController();
    let calls = 0;
    await rejects(() =>
      hydrateJavaScript({
        workspace,
        platformCheck: () => true,
        signal: controller.signal,
        verify: () => {
          calls++;
          controller.abort();
          return payload;
        },
      })
    );
    assert(calls === 1);
    await noReady(workspace);
  } finally {
    await Deno.remove(workspace, { recursive: true });
  }
});
