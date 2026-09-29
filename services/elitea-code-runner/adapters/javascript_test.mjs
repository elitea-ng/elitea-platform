import { executeJavaScript } from "./javascript.mjs";

async function execute(source, language = "javascript", input = {}) {
  return await executeJavaScript(
    source,
    input,
    language,
    "/tmp",
  );
}

Deno.test("JavaScript state is isolated and supports top-level await", async () => {
  const input = { count: 4 };
  const result = await execute(
    `
console.log('diagnostic');
await Promise.resolve();
alita_state.count = 99;
export default async (state) => ({ value: state.count + 1, legacy: alita_state.count });
`,
    "javascript",
    input,
  );
  if (result.value !== 5 || result.legacy !== 99 || input.count !== 4) {
    throw new Error("State contract failed");
  }
});

Deno.test("TypeScript types are transpiled and default values returned", async () => {
  const result = await execute(
    `
interface Result { value: number }
const answer: Result = { value: 42 };
export default answer;
`,
    "typescript",
  );
  if (result.value !== 42) throw new Error("TypeScript result failed");
});

Deno.test("missing, invalid, cyclic and oversized results fail explicitly", async () => {
  for (
    const [source, expected] of [
      ["export const value = 1;", "export a default"],
      ["export default { bad: NaN };", "finite JSON"],
      ["export default { bad: undefined };", "finite JSON"],
      ["const x = {}; x.self = x; export default x;", "circular"],
      ["export default 'x'.repeat(256 * 1024);", "structured result limit"],
      ["throw new Error('code-failed'); export default 1;", "code-failed"],
    ]
  ) {
    let failed = false;
    try {
      await execute(source);
    } catch (error) {
      if (!String(error).includes(expected)) throw error;
      failed = true;
    }
    if (!failed) throw new Error("Invalid execution accepted");
  }
});
