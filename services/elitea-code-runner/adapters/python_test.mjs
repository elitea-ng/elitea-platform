import { executePython } from "./python.mjs";

Deno.test("real Pyodide preserves state, final expression, await and source escaping", async () => {
  const cache = Deno.env.get("ELITEA_TEST_WHEEL_CACHE");
  if (!cache) {
    throw new Error("Set ELITEA_TEST_WHEEL_CACHE to the prepared wheel cache");
  }
  const input = { text: "literal\\nvalue", numbers: [2, 3] };
  const result = await executePython(
    String.raw`
import asyncio
await asyncio.sleep(0)
print('diagnostic only')
alita_state['text'] = 'legacy copy'
{'sum': sum(elitea_state['numbers']), 'text': elitea_state['text'], 'legacy': alita_state['text']}
`,
    input,
    cache,
  );
  if (
    JSON.stringify(result) !==
      JSON.stringify({ sum: 5, text: input.text, legacy: "legacy copy" })
  ) {
    throw new Error("Python state/result contract changed");
  }
  if (input.text !== "literal\\nvalue") throw new Error("Host state mutated");
});

Deno.test("Python exceptions fail execution instead of returning successful state", async () => {
  try {
    await executePython(
      "raise ValueError('expected-code-failure')",
      {},
      Deno.env.get("ELITEA_TEST_WHEEL_CACHE"),
    );
  } catch (error) {
    if (String(error).includes("expected-code-failure")) return;
    throw error;
  }
  throw new Error("Python failure was swallowed");
});

Deno.test("inline micropip installation works from the prepared offline cache", async () => {
  const result = await executePython(
    `
import micropip
await micropip.install('idna==3.10')
import idna
{'domain': idna.encode('bücher.example').decode(), 'version': idna.__version__}
`,
    {},
    Deno.env.get("ELITEA_TEST_WHEEL_CACHE"),
  );
  if (result.domain !== "xn--bcher-kva.example" || result.version !== "3.10") {
    throw new Error("Inline dependency installation failed");
  }
});

Deno.test("non-JSON and oversized results fail instead of producing truncated state", async () => {
  for (
    const [source, expected] of [
      ["float('nan')", "Out of range float"],
      ["'x' * (256 * 1024)", "structured result limit"],
    ]
  ) {
    try {
      await executePython(source, {}, Deno.env.get("ELITEA_TEST_WHEEL_CACHE"));
    } catch (error) {
      if (String(error).includes(expected)) continue;
      throw error;
    }
    throw new Error("Invalid result accepted");
  }
});
