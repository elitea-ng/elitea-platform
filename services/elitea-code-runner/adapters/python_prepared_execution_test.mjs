import { verifyPythonCodePackages } from "./prepare_python_code.mjs";
import { executePythonRequest } from "./python.mjs";

const [directory, expectedDigest] = Deno.args;
if (!directory || !expectedDigest) {
  throw new Error(
    "Provide the prepared bundle and its independently recorded digest",
  );
}

const calculation = `
from humanize import intcomma, naturalsize
values = [(i * 7919 + elitea_state['seed']) % 100000 + 100 for i in range(20000)]
{'records': len(values), 'total': sum(values), 'formatted': intcomma(sum(values)),
 'size': naturalsize(20000), 'status': 'PASS'}
`;
const expected = {
  records: 20000,
  total: 1001850000,
  formatted: "1,001,850,000",
  size: "20.0 kB",
  status: "PASS",
};

for (const mode of ["automatic", "inline range"]) {
  Deno.test(`resolved bundle supports offline ${mode} execution`, async () => {
    await verifyPythonCodePackages(directory, expectedDigest);
    const source = mode === "automatic"
      ? calculation
      : "import micropip\nawait micropip.install('humanize>=4.13,<4.14')\n" +
        calculation;
    const result = await executePythonRequest({
      revision: 2,
      language: "python",
      dependency_bundle_sha256: expectedDigest,
      source,
      input: { seed: 42 },
    }, directory);
    if (JSON.stringify(result) !== JSON.stringify(expected)) {
      throw new Error(
        `Prepared dependency calculation changed: ${JSON.stringify(result)}`,
      );
    }
    // The mounted content remains immutable after native package installation.
    await verifyPythonCodePackages(directory, expectedDigest);
  });
}

Deno.test("request verification rejects a substituted bundle before user code", async () => {
  try {
    await executePythonRequest({
      revision: 2,
      language: "python",
      dependency_bundle_sha256: "0".repeat(64),
      source: "raise RuntimeError('user code executed before verification')",
      input: {},
    }, directory);
  } catch (error) {
    if (!String(error).includes("content changed after resolution")) {
      throw error;
    }
    return;
  }
  throw new Error("Substituted dependency identity was accepted");
});

Deno.test("request verification rejects dependency identity downgrade", async () => {
  for (const revision of [1, 3]) {
    try {
      await executePythonRequest({
        revision,
        language: "python",
        dependency_bundle_sha256: expectedDigest,
        source: "raise RuntimeError('user code executed before verification')",
        input: {},
      }, directory);
    } catch (error) {
      if (!String(error).includes("request is invalid")) throw error;
      continue;
    }
    throw new Error("Dependency identity downgrade was accepted");
  }
});
