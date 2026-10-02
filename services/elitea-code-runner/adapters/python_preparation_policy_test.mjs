import { loadPyodide } from "npm:pyodide@0.29.0";
import { preparePythonPackages } from "./preload.mjs";

const template = Deno.args[0] ?? "/opt/elitea-wheels";
const cache = await Deno.makeTempDir({ dir: "/workspace" });
for await (const entry of Deno.readDir(template)) {
  if (entry.isFile) {
    await Deno.copyFile(`${template}/${entry.name}`, `${cache}/${entry.name}`);
  }
}
const python = await loadPyodide({ packageCacheDir: cache });

async function rejected(requirement, requireExactVersions, reason) {
  try {
    await preparePythonPackages(cache, [requirement], {
      interpreter: python,
      requireExactVersions,
    });
  } catch (error) {
    if (!reason.test(String(error))) throw error;
    return;
  }
  throw new Error("Invalid package preparation policy was accepted");
}

Deno.test("image profiles retain exact version policy by default", async () => {
  for (const requirement of ["humanize", "humanize>=4", "humanize==4.*"]) {
    // Passing undefined must retain the production default.
    await rejected(requirement, undefined, /require exact versions/);
  }
});
Deno.test("both preparation modes reject direct URLs and environment markers", async () => {
  for (const exact of [true, false]) {
    for (
      const requirement of [
        "humanize @ https://example.com/humanize.whl",
        "humanize==4.13.0; python_version >= '3'",
      ]
    ) {
      await rejected(requirement, exact, /without URLs or markers/);
    }
  }
});
Deno.test("invalid requirement syntax fails in the native parser", async () => {
  await rejected("not a requirement !", false, /InvalidRequirement/);
});
Deno.test("metadata requirements reject controls and Unicode before resolution", async () => {
  for (const requirement of ["", "humanize\n", "humanize\u2028", "humanizé"]) {
    await rejected(requirement, false, /bounded ASCII requirements/);
  }
});
