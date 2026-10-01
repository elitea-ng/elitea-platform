// Operator-owned image preparation. Generate a module graph; never execute it.
export function dependencyModule(packages) {
  if (!Array.isArray(packages) || packages.length > 128) {
    throw new Error(
      "JavaScript package profile must contain at most 128 requirements",
    );
  }
  const exact =
    /^npm:(?:@[a-z0-9][a-z0-9._-]*\/)?[a-z0-9][a-z0-9._-]*@(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-[a-z0-9.-]+)?$/;
  for (const requirement of packages) {
    if (
      typeof requirement !== "string" || requirement.length > 256 ||
      exact.exec(requirement)?.[0] !== requirement
    ) {
      throw new Error(
        "JavaScript packages require an exact npm package version",
      );
    }
  }
  return [...new Set(packages)].map((requirement) =>
    `import ${JSON.stringify(requirement)};`
  ).join("\n") + "\n";
}

if (import.meta.main) {
  const [profile, output] = Deno.args;
  if (!profile || !output || (await Deno.stat(profile)).size > 40 * 1024) {
    throw new Error("JavaScript package profile or output path is invalid");
  }
  const packages = JSON.parse(await Deno.readTextFile(profile));
  await Deno.writeTextFile(output, dependencyModule(packages));
}
