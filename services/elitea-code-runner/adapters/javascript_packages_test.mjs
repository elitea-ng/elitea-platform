import { executeJavaScript } from "./javascript.mjs";

const source = `
import stripAnsi from 'npm:strip-ansi@7.1.0';
import slugify from 'npm:slugify@1.6.6';
import { parse } from 'npm:csv-parse@5.6.0/sync';
export default state => {
  const records = parse(state.csv, { columns: true });
  return {
    clean: stripAnsi('\u001b[31mred\u001b[39m'),
    slug: slugify(records[0].name, { lower: true }),
    total: records.reduce((sum, row) => sum + Number(row.amount), 0),
  };
};
`;

for (const language of ["javascript", "typescript"]) {
  Deno.test(`${language} loads frozen npm dependencies offline`, async () => {
    const result = await executeJavaScript(
      source,
      { csv: "name,amount\nHello World,2\nSecond Row,3\n" },
      language,
      "/tmp",
    );
    if (
      result.clean !== "red" || result.slug !== "hello-world" ||
      result.total !== 5
    ) {
      throw new Error("Frozen npm state result differs");
    }
  });
}

Deno.test("an unprepared npm version cannot change the frozen package graph", async () => {
  let rejected = false;
  try {
    await executeJavaScript(
      "import slugify from 'npm:slugify@1.6.5'; export default slugify('A B');",
      {},
      "javascript",
      "/tmp",
    );
  } catch {
    rejected = true;
  }
  if (!rejected) throw new Error("Unprepared npm version executed");
});
