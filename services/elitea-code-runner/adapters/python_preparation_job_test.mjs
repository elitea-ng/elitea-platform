import {
  decodePreparationJob,
  preparationFailure,
  runPythonPreparation,
} from "./python_preparation_job.mjs";

const encoder = new TextEncoder();
const jobName = ".elitea-code.json";
const markerName = ".elitea-python-preparation.json";
const releaseName = ".elitea-python-preparation-release";
const nativeName = "elitea-python-bundle.json";
const source =
  'raise RuntimeError("source must never execute")\n# \\"},"revision":2';
const bundle = {
  revision: 1,
  runtime: "pyodide-0.29.0",
  requirements: ["humanize==4.13.0"],
  files: [{
    name: "elitea-python-lock.json",
    bytes: 2,
    sha256: "a".repeat(64),
  }],
  digest: "b".repeat(64),
};
function job() {
  return {
    revision: 1,
    language: "python",
    source,
    preparer_image_digest: `sha256:${"a".repeat(64)}`,
    policy_revision: "python-v1",
    timeout_seconds: 3,
  };
}
function assert(condition, message = "Assertion failed") {
  if (!condition) throw new Error(message);
}
function equal(actual, expected) {
  assert(JSON.stringify(actual) === JSON.stringify(expected), "Values differ");
}
async function rejects(operation, code) {
  try {
    await operation();
  } catch (error) {
    if (code) equal(preparationFailure(error).error, code);
    return error;
  }
  throw new Error("Invalid preparation was accepted");
}
function decode(value) {
  return decodePreparationJob(
    encoder.encode(typeof value === "string" ? value : JSON.stringify(value)),
  );
}

async function fixture(test) {
  const workspace = await Deno.makeTempDir({ dir: "/private/tmp" });
  let elapsed = 0;
  const calls = { prepare: 0, verify: 0, sleep: 0 };
  const clock = {
    wallNow: () => 1800000000000 + elapsed,
    monotonicNow: () => elapsed,
    setTimer: (callback, milliseconds) => setTimeout(callback, milliseconds),
    clearTimer: (timer) => clearTimeout(timer),
  };
  const marker = async () =>
    JSON.parse(await Deno.readTextFile(`${workspace}/${markerName}`));
  const release = async () =>
    await Deno.writeFile(`${workspace}/${releaseName}`, new Uint8Array());
  const options = {
    workspace,
    clock,
    prepare: async (directory, receivedSource) => {
      calls.prepare++;
      equal(directory, `${workspace}/python-dependencies`);
      equal(receivedSource, source);
      await rejects(() => Deno.lstat(`${workspace}/${markerName}`));
      await Deno.writeTextFile(
        `${directory}/${nativeName}`,
        JSON.stringify(bundle),
      );
      return structuredClone(bundle);
    },
    verify: async (directory, root) => {
      calls.verify++;
      equal(root, bundle.digest);
      return JSON.parse(await Deno.readTextFile(`${directory}/${nativeName}`));
    },
    sleep: async (milliseconds) => {
      calls.sleep++;
      equal((await marker()).bundle, bundle);
      elapsed += milliseconds;
      await release();
    },
  };
  try {
    await Deno.writeTextFile(`${workspace}/${jobName}`, JSON.stringify(job()));
    await test({
      workspace,
      calls,
      clock,
      options,
      marker,
      release,
      advance: (ms) => elapsed += ms,
    });
  } finally {
    await Deno.remove(workspace, { recursive: true });
  }
}

Deno.test("strict preparation transport preserves literal source and field order independence", () => {
  equal(decode(job()), job());
  equal(decode(JSON.stringify(job(), [...Object.keys(job())].reverse())), {
    ...Object.fromEntries(Object.entries(job()).reverse()),
  });
  equal(decode({ ...job(), source: "\ufeff" }).source, "\ufeff");
  for (const timeout_seconds of [1, 3600]) {
    equal(
      decode({ ...job(), timeout_seconds }).timeout_seconds,
      timeout_seconds,
    );
  }
});

Deno.test("preparation transport rejects unknown, duplicate, and execution fields", async () => {
  const serialized = JSON.stringify(job());
  for (
    const field of [
      "input",
      "packages",
      "grant",
      "endpoint",
      "argv",
      "image_digest",
    ]
  ) {
    await rejects(
      () => decode({ ...job(), [field]: {} }),
      "invalid_preparation",
    );
  }
  for (const field of Object.keys(job())) {
    const value = JSON.stringify(job()[field]);
    await rejects(
      () =>
        decode(serialized.replace("{", `{${JSON.stringify(field)}:${value},`)),
      "invalid_preparation",
    );
  }
  await rejects(
    () => decode(serialized.replace("{", '{"\\u0072evision":1,')),
    "invalid_preparation",
  );
  await rejects(
    () => decode(serialized.replace('"revision":1', '"revision":1.0')),
    "invalid_preparation",
  );
  await rejects(
    () =>
      decode(
        serialized.replace('"timeout_seconds":3', '"timeout_seconds":3e0'),
      ),
    "invalid_preparation",
  );
});

Deno.test("preparation transport applies source, identity, policy, and timeout bounds", async () => {
  for (
    const [field, value] of [
      ["revision", 2],
      ["language", "javascript"],
      ["source", ""],
      ["source", "\u0085"],
      ["source", "\0"],
      ["source", "\ud800"],
      ["source", "é".repeat(128 * 1024 + 1)],
      ["preparer_image_digest", "python:latest"],
      ["preparer_image_digest", `sha256:${"a".repeat(64)}\n`],
      ["preparer_image_digest", `sha256:${"A".repeat(64)}`],
      ["policy_revision", ""],
      ["policy_revision", "python v1"],
      ["policy_revision", "python-v1\n"],
      ["policy_revision", "x".repeat(129)],
      ["timeout_seconds", 0],
      ["timeout_seconds", 3601],
      ["timeout_seconds", 1.5],
      ["timeout_seconds", "3"],
      ["source", null],
    ]
  ) {
    await rejects(
      () => decode({ ...job(), [field]: value }),
      "invalid_preparation",
    );
  }
  equal(
    decode({ ...job(), source: "é".repeat(128 * 1024) }).source.length,
    128 * 1024,
  );
  await rejects(
    () => decodePreparationJob(new Uint8Array(1024 * 1024 + 1)),
    "invalid_preparation",
  );
  await rejects(
    () => decodePreparationJob(new Uint8Array([0xff])),
    "invalid_preparation",
  );
  await rejects(
    () => decode("\ufeff" + JSON.stringify(job())),
    "invalid_preparation",
  );
});

Deno.test("native bundle validation precedes atomic marker publication and holding", async () => {
  await fixture(async ({ workspace, calls, options, marker }) => {
    const result = await runPythonPreparation(options);
    equal(calls, { prepare: 1, verify: 1, sleep: 1 });
    equal(result, await marker());
    equal(result.status, "resolved");
    equal(result.source_sha256.length, 64);
    equal(result.preparer_image_digest, job().preparer_image_digest);
    equal(result.policy_revision, job().policy_revision);
    equal(result.deadline_unix_ms - result.started_unix_ms, 3000);
    equal(
      (await Deno.lstat(`${workspace}/python-dependencies`)).mode & 0o777,
      0o700,
    );
    equal((await Deno.lstat(`${workspace}/${markerName}`)).mode & 0o777, 0o600);
    await rejects(() => Deno.lstat(`${workspace}/${markerName}.tmp`));
    assert(!JSON.stringify(result).includes(source));
    assert(!Object.hasOwn(result, "input"));
  });
});

Deno.test("successful preparation remains alive while the supervisor copies native files", async () => {
  await fixture(async ({ workspace, options, release, calls }) => {
    let releaseWait;
    let enteredHold;
    const inHold = new Promise((resolve) => enteredHold = resolve);
    options.sleep = () => {
      enteredHold();
      return new Promise((resolve) => releaseWait = resolve);
    };
    let completed = false;
    const process = runPythonPreparation(options).then(() => completed = true);
    await inHold;
    assert(!completed);
    equal(calls.verify, 1);
    equal(
      JSON.parse(
        await Deno.readTextFile(
          `${workspace}/python-dependencies/${nativeName}`,
        ),
      ),
      bundle,
    );
    await release();
    releaseWait();
    await process;
    assert(completed);
  });
});

Deno.test("restart verifies recorded bytes without resolution or deadline renewal", async () => {
  await fixture(async ({ workspace, calls, options, marker, advance }) => {
    await runPythonPreparation(options);
    const original = await marker();
    await Deno.remove(`${workspace}/${releaseName}`);
    advance(1500);
    equal(await runPythonPreparation(options), original);
    equal(calls.prepare, 1);
    equal(calls.verify, 2);
    equal(await marker(), original);
  });
});

Deno.test("expired restart cannot resolve or verify the native bundle", async () => {
  await fixture(async ({ calls, options, advance }) => {
    await runPythonPreparation(options);
    advance(3000);
    await rejects(() => runPythonPreparation(options), "deadline_exceeded");
    equal(calls.prepare, 1);
    equal(calls.verify, 1);
  });
});

Deno.test("reuse rejects changed source, image, policy, and timeout", async () => {
  for (
    const change of [
      { source: "pass" },
      { preparer_image_digest: `sha256:${"c".repeat(64)}` },
      { policy_revision: "python-v2" },
      { timeout_seconds: 4 },
    ]
  ) {
    await fixture(async ({ workspace, calls, options }) => {
      await runPythonPreparation(options);
      await Deno.writeTextFile(
        `${workspace}/${jobName}`,
        JSON.stringify({ ...job(), ...change }),
      );
      await rejects(() => runPythonPreparation(options), "invalid_reuse");
      equal(calls.prepare, 1);
      equal(calls.verify, 1);
    });
  }
});

Deno.test("partial directories, stale markers, and stale releases cannot trigger resolution", async () => {
  for (const existing of ["directory", "temporary", "release", "marker"]) {
    await fixture(async ({ workspace, calls, options }) => {
      if (existing === "directory") {
        await Deno.mkdir(`${workspace}/python-dependencies`);
      }
      if (existing === "temporary") {
        await Deno.writeTextFile(`${workspace}/${markerName}.tmp`, "{}");
      }
      if (existing === "release") {
        await Deno.writeFile(`${workspace}/${releaseName}`, new Uint8Array());
      }
      if (existing === "marker") {
        await Deno.writeTextFile(`${workspace}/${markerName}`, "{}");
      }
      await rejects(() => runPythonPreparation(options), "invalid_reuse");
      equal(calls.prepare, 0);
      equal(calls.verify, 0);
    });
  }
});

Deno.test("native verification failure keeps the success marker absent", async () => {
  await fixture(async ({ workspace, calls, options }) => {
    options.verify = () => {
      throw new Error(
        "private requirement https://registry.invalid/token-secret",
      );
    };
    const error = await rejects(
      () => runPythonPreparation(options),
      "preparation_failed",
    );
    equal(preparationFailure(error), {
      revision: 1,
      status: "failed",
      error: "preparation_failed",
    });
    await rejects(() => Deno.lstat(`${workspace}/${markerName}`));
    equal(calls.prepare, 1);
    await rejects(() => runPythonPreparation(options), "invalid_reuse");
    equal(calls.prepare, 1);
  });
});

Deno.test("tampered native bundle reuse fails without a second resolution", async () => {
  await fixture(async ({ workspace, calls, options }) => {
    await runPythonPreparation(options);
    await Deno.writeTextFile(
      `${workspace}/python-dependencies/${nativeName}`,
      JSON.stringify({ ...bundle, digest: "c".repeat(64) }),
    );
    await rejects(() => runPythonPreparation(options), "invalid_reuse");
    equal(calls.prepare, 1);
    equal(calls.verify, 2);
  });
});

Deno.test("unbounded or unsafe native metadata is never published", async () => {
  for (
    const change of [
      { digest: bundle.digest + "\n" },
      { runtime: "other" },
      { extra: "https://registry.invalid" },
      { requirements: ["humanize @ https://registry.invalid/package.whl"] },
      { requirements: ["humanize\n"] },
      { requirements: ["x".repeat(257)] },
      { files: [{ ...bundle.files[0], name: "../lock.json" }] },
      { files: [{ ...bundle.files[0], bytes: 32 * 1024 * 1024 + 1 }] },
      { files: [bundle.files[0], bundle.files[0]] },
    ]
  ) {
    await fixture(async ({ workspace, options, calls }) => {
      options.prepare = () => ({ ...structuredClone(bundle), ...change });
      await rejects(() => runPythonPreparation(options), "invalid_bundle");
      await rejects(() => Deno.lstat(`${workspace}/${markerName}`));
      equal(calls.verify, 0);
    });
  }
});

Deno.test("resolution time counts against the original deadline before publication", async () => {
  await fixture(async ({ workspace, options, advance, calls }) => {
    const prepare = options.prepare;
    options.prepare = async (...args) => {
      const resolved = await prepare(...args);
      advance(3001);
      return resolved;
    };
    await rejects(() => runPythonPreparation(options), "deadline_exceeded");
    await rejects(() => Deno.lstat(`${workspace}/${markerName}`));
    equal(calls.verify, 0);
  });
});

Deno.test("an unresolved native operation reaches the bounded deadline", async () => {
  await fixture(async ({ workspace, options, advance }) => {
    options.prepare = () => new Promise(() => {});
    options.clock.setTimer = (callback, milliseconds) => {
      queueMicrotask(() => {
        advance(milliseconds);
        callback();
      });
      return null;
    };
    await rejects(() => runPythonPreparation(options), "deadline_exceeded");
    await rejects(() => Deno.lstat(`${workspace}/${markerName}`));
  });
});

Deno.test("holding consumes only the deadline left after native resolution", async () => {
  await fixture(async ({ options, advance, calls, marker }) => {
    const prepare = options.prepare;
    options.prepare = async (...args) => {
      const result = await prepare(...args);
      advance(2500);
      return result;
    };
    const waits = [];
    options.sleep = (milliseconds) => {
      waits.push(milliseconds);
      advance(milliseconds);
    };
    await rejects(() => runPythonPreparation(options), "deadline_exceeded");
    equal(waits, [100, 100, 100, 100, 100]);
    equal(
      (await marker()).deadline_unix_ms - (await marker()).started_unix_ms,
      3000,
    );
    equal(calls.prepare, 1);
  });
});

Deno.test("release requires an empty regular file", async () => {
  for (const kind of ["content", "directory", "symlink"]) {
    await fixture(async ({ workspace, options }) => {
      options.sleep = async () => {
        const path = `${workspace}/${releaseName}`;
        if (kind === "content") await Deno.writeTextFile(path, "release");
        if (kind === "directory") await Deno.mkdir(path);
        if (kind === "symlink") {
          await Deno.symlink(`${workspace}/${jobName}`, path);
        }
      };
      await rejects(() => runPythonPreparation(options), "invalid_release");
    });
  }
});

Deno.test("symlink jobs and package directories fail safely", async () => {
  await fixture(async ({ workspace, options, calls }) => {
    await Deno.rename(`${workspace}/${jobName}`, `${workspace}/real.json`);
    await Deno.symlink(`${workspace}/real.json`, `${workspace}/${jobName}`);
    await rejects(
      () => runPythonPreparation(options),
      "invalid_preparation_file",
    );
    equal(calls.prepare, 0);
  });
  await fixture(async ({ workspace, options, calls }) => {
    await Deno.symlink(workspace, `${workspace}/python-dependencies`);
    await rejects(() => runPythonPreparation(options), "invalid_reuse");
    equal(calls.prepare, 0);
  });
});
