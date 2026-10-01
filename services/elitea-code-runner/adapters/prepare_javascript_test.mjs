import { dependencyModule } from "./prepare_javascript.mjs";

Deno.test("package preparation emits only exact npm module references", () => {
  const source = dependencyModule([
    "npm:slugify@1.6.6",
    "npm:@scope/package@1.2.3-beta.1",
    "npm:slugify@1.6.6",
  ]);
  if (
    source !==
      'import "npm:slugify@1.6.6";\nimport "npm:@scope/package@1.2.3-beta.1";\n'
  ) {
    throw new Error("Prepared dependency module differs");
  }
});

Deno.test("package preparation rejects unpinned and non-registry sources", () => {
  for (
    const packages of [
      {},
      [null],
      ["npm:slugify@latest"],
      ["npm:slugify@^1.6.6"],
      ["npm:slugify@1.6.6/../../evil"],
      ["npm:slugify@01.6.6"],
      ["https://example.com/a.mjs"],
      ["file:///workspace/code.mjs"],
      ["npm:slugify@1.6.6\nexport default 42;"],
      ["npm:slugify@1.6.6\n"],
      Array(129).fill("npm:slugify@1.6.6"),
    ]
  ) {
    let rejected = false;
    try {
      dependencyModule(packages);
    } catch {
      rejected = true;
    }
    if (!rejected) throw new Error("Invalid package profile accepted");
  }
});
