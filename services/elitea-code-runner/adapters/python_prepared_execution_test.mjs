import { verifyPythonCodePackages } from "./prepare_python_code.mjs";
import { executePython } from "./python.mjs";

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
    const result = await executePython(source, { seed: 42 }, directory);
    if (JSON.stringify(result) !== JSON.stringify(expected)) {
      throw new Error(
        `Prepared dependency calculation changed: ${JSON.stringify(result)}`,
      );
    }
    // The mounted content remains immutable after native package installation.
    await verifyPythonCodePackages(directory, expectedDigest);
  });
}
