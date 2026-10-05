import plugin, {
  prepareJavaScriptCodePackages,
  verifyJavaScriptCodePackages,
} from "./prepare_javascript_code.mjs";

const encoder = new TextEncoder();
const nativeTests = Deno.args.includes("--native");
function assert(value, message = "Assertion failed") {
  if (!value) throw new Error(message);
}
async function rejects(operation) {
  let rejected = false;
  try {
    await operation();
  } catch {
    rejected = true;
  }
  assert(rejected, "Unsafe preparation or reuse succeeded");
}
async function directoryTest(operation) {
  const directory = await Deno.makeTempDir({
    prefix: "elitea-javascript-preparation-test-",
  });
  try {
    await operation(directory);
  } finally {
    await Deno.remove(directory, { recursive: true });
  }
}
function inspect(source, language = "typescript") {
  const diagnostics = Deno.lint.runPlugin(
    plugin,
    language === "typescript" ? "source.ts" : "source.mjs",
    source,
  );
  assert(diagnostics.length === 1, "Native AST inspection marker is missing");
  assert(diagnostics[0].id === "elitea-dependencies/record");
  return JSON.parse(diagnostics[0].message);
}
async function sha256(bytes) {
  return Array.from(
    new Uint8Array(await crypto.subtle.digest("SHA-256", bytes)),
  )
    .map((byte) => byte.toString(16).padStart(2, "0")).join("");
}

Deno.test("native AST extracts static npm and JSR roots, subpaths, ranges, and type imports", () => {
  const record = inspect(`
    import number from 'npm:is-number';
    import type { X } from 'jsr:@std/bytes@^1.0.4';
    export { parse } from 'npm:csv-parse@5.6.0/sync';
    export * from 'npm:is-number';
    type Y = import('npm:@types/node@24.0.0').Buffer;
    import 'node:crypto';
    const marker = '💾 import("https://example.invalid/") deno-lint-ignore';
    // import("npm:unrequested")
    export default number;
  `);
  assert(record.rejected === false);
  assert(
    JSON.stringify(record.requirements) === JSON.stringify([
      "jsr:@std/bytes@^1.0.4",
      "npm:@types/node@24.0.0",
      "npm:csv-parse@5.6.0/sync",
      "npm:is-number",
    ]),
  );
});

Deno.test("native AST rejects dynamic, local, URL, bare, and registry configuration imports", () => {
  for (
    const source of [
      'import("npm:is-number@7.0.0");',
      "import(name);",
      'import("npm:" + name);',
      "import(`npm:${name}`);",
      "const f = () => import(name);",
      'import x = require("npm:is-number@7.0.0");',
      'require("npm:is-number@7.0.0");',
      'import "https://example.invalid/module.ts";',
      'export * from "file:///tmp/module.ts";',
      'import "data:text/javascript,export default 1";',
      'import "./local.mjs";',
      'import "is-number";',
      'import "npm:pkg@https://example.invalid/file.tgz";',
      'import "npm:pkg@1.0.0/../other";',
      'import "jsr:@std/bytes@1.0.4/../../other";',
      '// @deno-types="https://example.invalid/types.d.ts"\nexport default 1;',
      '/// <reference types="https://example.invalid/types.d.ts" />\nexport default 1;',
      "// @jsxImportSource https://example.invalid\nexport default 1;",
      Array.from({ length: 129 }, (_, n) => `import "npm:package-${n}@1.0.0";`)
        .join("\n"),
    ]
  ) assert(inspect(source).rejected === true, "Source policy was bypassed");
});

Deno.test("production CLI fails closed for disabled inspection and syntax errors", async () => {
  for (
    const source of [
      "// deno-lint-ignore-file\nimport(name);",
      "// deno-lint-ignore-file elitea-dependencies/record\nexport default 1;",
      "// deno-lint-ignore elitea-dependencies/record\nimport(name);",
      "/* deno-lint-ignore-file */\nimport(name);",
      "const broken = ;",
      'import "https://example.invalid/module.ts"; export default 1;',
    ]
  ) {
    await directoryTest(async (directory) => {
      await rejects(() => prepareJavaScriptCodePackages(directory, source));
      await rejects(() =>
        Deno.stat(`${directory}/elitea-javascript-bundle.json`)
      );
    });
  }
});

Deno.test("production CLI accepts Unicode and import text only as data without executing source", async () => {
  await directoryTest(async (directory) => {
    const marker = `${directory}/source-executed`;
    const source = `await Deno.writeTextFile(${
      JSON.stringify(marker)
    }, "executed");
      const text = '💾 import("https://example.invalid/") deno-lint-ignore';
      export default text;`;
    const bundle = await prepareJavaScriptCodePackages(directory, source);
    assert(bundle.requirements.length === 0);
    await rejects(() => Deno.stat(marker));
    const reused = await verifyJavaScriptCodePackages(directory, bundle.digest);
    assert(JSON.stringify(reused) === JSON.stringify(bundle));
    await rejects(() => Deno.stat(marker));
  });
});

Deno.test("source, language, deadline, staging, and byte limits reject before native resolution", async () => {
  for (
    const [source, language, options] of [
      ["", "javascript", {}],
      [" ", "typescript", {}],
      ["export default 1;", "python", {}],
      ["\ud800", "javascript", {}],
      ["💾".repeat(65537), "javascript", {}],
      ["export default 1;", "javascript", { timeoutSeconds: 0 }],
      ["export default 1;", "javascript", { timeoutSeconds: 121 }],
      ["export default 1;", "javascript", { timeoutSeconds: 1.5 }],
    ]
  ) {
    await directoryTest(async (directory) => {
      await rejects(() =>
        prepareJavaScriptCodePackages(directory, source, language, options)
      );
      let count = 0;
      for await (const _entry of Deno.readDir(directory)) count++;
      assert(count === 0);
    });
  }
  await directoryTest(async (directory) => {
    await Deno.chmod(directory, 0o755);
    await rejects(() =>
      prepareJavaScriptCodePackages(directory, "export default 1;")
    );
    await Deno.chmod(directory, 0o700);
    await Deno.writeTextFile(`${directory}/partial`, "partial");
    await rejects(() =>
      prepareJavaScriptCodePackages(directory, "export default 1;")
    );
  });
});

Deno.test("reuse rejects tampered bytes, symlinks, unrecorded files, and preparation in place", async () => {
  for (const mutation of ["tamper", "symlink", "extra", "oversize"]) {
    await directoryTest(async (directory) => {
      const bundle = await prepareJavaScriptCodePackages(
        directory,
        "export default 1;",
      );
      await rejects(() =>
        prepareJavaScriptCodePackages(directory, "export default 2;")
      );
      const path = `${directory}/elitea-javascript-dependencies.mjs`;
      if (mutation === "tamper") await Deno.writeTextFile(path, " ");
      if (mutation === "extra") {
        await Deno.writeTextFile(`${directory}/extra`, "extra");
      }
      if (mutation === "oversize") {
        using file = await Deno.open(`${directory}/large`, {
          createNew: true,
          write: true,
        });
        await file.truncate(32 * 1024 * 1024 + 1);
      }
      if (mutation === "symlink") {
        await Deno.rename(path, `${directory}/outside`);
        await Deno.symlink(`${directory}/outside`, path);
      }
      await rejects(() =>
        verifyJavaScriptCodePackages(directory, bundle.digest)
      );
    });
  }
});

Deno.test("reuse rejects unsafe hydration metadata even when its content root is recomputed", async () => {
  for (
    const name of [
      "deno-cache/npm/../../escape",
      "/absolute",
      "deno-cache/npm/link",
      "../escape",
    ]
  ) {
    await directoryTest(async (directory) => {
      const bundle = await prepareJavaScriptCodePackages(
        directory,
        "export default 1;",
      );
      bundle.files[0].name = name;
      const content = {
        revision: bundle.revision,
        runtime: bundle.runtime,
        requirements: bundle.requirements,
        files: bundle.files,
      };
      bundle.digest = await sha256(encoder.encode(JSON.stringify(content)));
      await Deno.writeTextFile(
        `${directory}/elitea-javascript-bundle.json`,
        JSON.stringify(bundle),
      );
      await rejects(() =>
        verifyJavaScriptCodePackages(directory, bundle.digest)
      );
    });
  }
});

Deno.test("job capacity bounds concurrent native children", async () => {
  const directories = await Promise.all(
    Array.from(
      { length: 3 },
      () => Deno.makeTempDir({ prefix: "elitea-javascript-capacity-test-" }),
    ),
  );
  try {
    const operations = directories.map((directory) =>
      prepareJavaScriptCodePackages(directory, "export default 1;")
    );
    const results = await Promise.allSettled(operations);
    assert(
      results.filter((result) => result.status === "fulfilled").length === 2,
    );
    assert(
      results[2].status === "rejected" &&
        results[2].reason.message.includes("capacity"),
    );
  } finally {
    for (const directory of directories) {
      await Deno.remove(directory, { recursive: true });
    }
  }
});

async function nativeRun(directory, args) {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), 10000);
  try {
    return await new Deno.Command(Deno.execPath(), {
      args,
      cwd: directory,
      clearEnv: true,
      env: {
        DENO_DIR: `${directory}/deno-cache`,
        DENO_NO_UPDATE_CHECK: "1",
        NO_COLOR: "1",
      },
      signal: controller.signal,
    }).output();
  } finally {
    clearTimeout(timer);
  }
}

Deno.test({
  name:
    "native npm and JSR packages absent from the image profile prepare and execute offline",
  ignore: !nativeTests,
  async fn() {
    await directoryTest(async (directory) => {
      const profile = JSON.parse(
        await Deno.readTextFile(
          new URL("./javascript-packages.json", import.meta.url),
        ),
      );
      assert(
        !profile.some((value) =>
          value.startsWith("npm:is-number") ||
          value.startsWith("jsr:@std/bytes")
        ),
      );
      const source = `
        import isNumber from "npm:is-number@^7.0.0";
        import {concat} from "jsr:@std/bytes@1.0.4/concat";
        await Deno.writeTextFile(${
        JSON.stringify(`${directory}/executed`)
      }, "executed");
        const result: boolean = isNumber(concat([new Uint8Array([4,2])])[0]);
        console.log(JSON.stringify({result}));
        export default result;
      `;
      const empty = await Deno.makeTempDir({
        prefix: "elitea-javascript-cold-test-",
      });
      try {
        await Deno.writeTextFile(`${empty}/source.ts`, source);
        const miss = await nativeRun(empty, [
          "test",
          "--no-run",
          "--no-check",
          "--no-config",
          "--no-lock",
          "--cached-only",
          "--node-modules-dir=none",
          `${empty}/source.ts`,
        ]);
        assert(
          miss.code !== 0,
          "Package graph unexpectedly existed in the private cache",
        );
      } finally {
        await Deno.remove(empty, { recursive: true });
      }
      const bundle = await prepareJavaScriptCodePackages(
        directory,
        source,
        "typescript",
        { timeoutSeconds: 60 },
      );
      await rejects(() => Deno.stat(`${directory}/executed`));
      assert(bundle.requirements.length === 2);
      const lock = JSON.parse(
        await Deno.readTextFile(`${directory}/elitea-javascript-lock.json`),
      );
      assert(lock.npm["is-number@7.0.0"].integrity.startsWith("sha512-"));
      assert(lock.jsr["@std/bytes@1.0.4"].integrity.length === 64);
      const reuse = await verifyJavaScriptCodePackages(
        directory,
        bundle.digest,
      );
      assert(reuse.digest === bundle.digest);
      await rejects(() => Deno.stat(`${directory}/executed`));
      const execution = await Deno.makeTempDir({
        prefix: "elitea-javascript-execution-test-",
      });
      try {
        // This separate probe intentionally executes source only after preparation checks.
        await Deno.writeTextFile(
          `${execution}/source.ts`,
          source.replace("const result: boolean", "const result"),
        );
        const output = await nativeRun(directory, [
          "run",
          "--no-config",
          "--no-prompt",
          "--cached-only",
          "--frozen",
          "--node-modules-dir=none",
          "--deny-net",
          "--allow-write=" + `${directory}/executed`,
          "--lock=" + `${directory}/elitea-javascript-lock.json`,
          `${execution}/source.ts`,
        ]);
        assert(output.code === 0, new TextDecoder().decode(output.stderr));
        assert(
          JSON.parse(new TextDecoder().decode(output.stdout)).result === true,
        );
      } finally {
        await Deno.remove(execution, { recursive: true });
      }
    });
  },
});

Deno.test({
  name: "native preparation leaves npm lifecycle scripts unexecuted",
  ignore: !nativeTests,
  async fn() {
    await directoryTest(async (directory) => {
      const bundle = await prepareJavaScriptCodePackages(
        directory,
        'import esbuild from "npm:esbuild@0.25.10"; export default esbuild;',
        "javascript",
        { timeoutSeconds: 60 },
      );
      const packageDirectory =
        `${directory}/deno-cache/npm/registry.npmjs.org/esbuild/0.25.10`;
      const manifest = JSON.parse(
        await Deno.readTextFile(`${packageDirectory}/package.json`),
      );
      assert(manifest.scripts.postinstall === "node install.js");
      // The native postinstall replaces this JavaScript launcher with its executable.
      const launcher = await Deno.readTextFile(
        `${packageDirectory}/bin/esbuild`,
      );
      assert(launcher.startsWith("#!/usr/bin/env node\n"));
      assert(launcher.includes('"use strict"'));
      assert(!(await Deno.lstat(`${directory}/deno-cache/npm`)).isSymlink);
      assert(
        (await verifyJavaScriptCodePackages(directory, bundle.digest))
          .digest === bundle.digest,
      );
    });
  },
});
