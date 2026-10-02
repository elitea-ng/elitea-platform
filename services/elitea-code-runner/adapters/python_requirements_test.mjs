import { loadPyodide } from "npm:pyodide@0.29.0";
import { discoverPythonRequirements } from "./python_requirements.mjs";

const cache = Deno.args[0] ?? "/opt/elitea-wheels";
const python = await loadPyodide({ packageCacheDir: cache });
function same(actual, expected) {
  if (JSON.stringify(actual) !== JSON.stringify(expected)) {
    throw new Error(`Unexpected dependency plan: ${JSON.stringify(actual)}`);
  }
}
function rejected(source) {
  try {
    discoverPythonRequirements(python, source);
  } catch {
    return;
  }
  throw new Error("Invalid dependency source was accepted");
}
Deno.test("literal micropip requirements preserve order and aliases", () => {
  same(
    discoverPythonRequirements(
      python,
      `
import micropip as packages
await packages.install(['humanize==4.13.0', 'python-slugify==8.0.4'])
from micropip import install as add
await add(requirements='idna==3.10')
await packages.install('humanize==4.13.0')
raise RuntimeError('USER SOURCE MUST NOT EXECUTE')
`,
    ),
    {
      requirements: ["humanize==4.13.0", "python-slugify==8.0.4", "idna==3.10"],
      dynamic_installs: false,
      automatic_imports: false,
    },
  );
});
Deno.test("ordinary imports use the native distribution mapping", () => {
  same(discoverPythonRequirements(python, "import json\nimport sklearn\n"), {
    requirements: ["scikit-learn"],
    dynamic_installs: false,
    automatic_imports: true,
  });
});
Deno.test("dynamic installation expressions are never evaluated", () => {
  same(
    discoverPythonRequirements(
      python,
      `
import micropip
await micropip.install(__import__('pathlib').Path('/tmp/should-not-read').read_text())
`,
    ),
    {
      requirements: [],
      dynamic_installs: true,
      automatic_imports: false,
    },
  );
});
Deno.test("discovery rejects unsupported options and bounded syntax", () => {
  rejected(
    "import micropip\nawait micropip.install('idna', index_urls=['https://example.com'])",
  );
});
Deno.test("source and literal requirement bounds reject before preparation", () => {
  rejected("x".repeat(256 * 1024 + 1));
  rejected(
    "import micropip\nawait micropip.install('" + "a".repeat(257) + "')",
  );
  rejected(
    "import micropip\nawait micropip.install([" +
      Array.from({ length: 129 }, (_, i) => `'pkg${i}'`).join(",") + "])",
  );
  rejected("invalid Python:");
  rejected("import micropip\nawait micropip.install('package\\x00')");
  rejected("x\n".repeat(20001));
});
