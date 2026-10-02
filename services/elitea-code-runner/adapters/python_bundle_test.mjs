import {
  preparePythonCodePackages,
  verifyPythonCodePackages,
} from "./prepare_python_code.mjs";

const [directory, expectedDigest] = Deno.args;
if (!directory || !expectedDigest) {
  throw new Error(
    "Provide the private prepared bundle and its recorded digest",
  );
}
async function rejects(operation, reason) {
  try {
    await operation();
  } catch (error) {
    if (reason && !reason.test(String(error))) throw error;
    return;
  }
  throw new Error("Altered package content was accepted");
}
async function privateCopy() {
  const target = await Deno.makeTempDir({ dir: "/workspace" });
  for await (const entry of Deno.readDir(directory)) {
    if (entry.isFile) {
      await Deno.copyFile(
        `${directory}/${entry.name}`,
        `${target}/${entry.name}`,
      );
    }
  }
  return target;
}
Deno.test("bundle reuse is offline and binds the previously recorded digest", async () => {
  const result = await verifyPythonCodePackages(directory, expectedDigest);
  if (result.requirements[0] !== "humanize==4.13.0") {
    throw new Error("Wrong prepared requirement");
  }
  await rejects(() => verifyPythonCodePackages(directory, "0".repeat(64)));
  await rejects(() =>
    verifyPythonCodePackages(directory, expectedDigest + "\n")
  );
});
Deno.test("modified wheel bytes cannot reuse the original receipt", async () => {
  const copy = await privateCopy();
  const path = `${copy}/humanize-4.13.0-py3-none-any.whl`;
  const bytes = await Deno.readFile(path);
  bytes[0] ^= 1;
  await Deno.writeFile(path, bytes);
  await rejects(
    () => verifyPythonCodePackages(copy, expectedDigest),
    /wheel does not match the native lock/,
  );
});
Deno.test("symlink and unsafe lock paths are rejected", async () => {
  const copy = await privateCopy();
  const path = `${copy}/humanize-4.13.0-py3-none-any.whl`;
  await Deno.rename(path, path + ".real");
  await Deno.symlink(path + ".real", path);
  await rejects(
    () => verifyPythonCodePackages(copy, expectedDigest),
    /bounded regular file/,
  );
  for (const unsafeName of ["../humanize.whl", "humanize.whl\n"]) {
    const changed = await privateCopy();
    const lockPath = `${changed}/elitea-python-lock.json`;
    const lock = JSON.parse(await Deno.readTextFile(lockPath));
    lock.packages.humanize.file_name = unsafeName;
    await Deno.writeTextFile(lockPath, JSON.stringify(lock));
    await rejects(
      () => verifyPythonCodePackages(changed, expectedDigest),
      /invalid content references/,
    );
  }
});
Deno.test("oversized metadata is rejected before decoding", async () => {
  const copy = await privateCopy();
  await Deno.writeFile(
    `${copy}/elitea-python-bundle.json`,
    new Uint8Array(128 * 1024 + 1),
  );
  await rejects(() => verifyPythonCodePackages(copy, expectedDigest));
});
Deno.test("an existing bundle cannot be resolved again in place", async () => {
  await rejects(
    () =>
      preparePythonCodePackages(
        directory,
        "raise RuntimeError('do not execute')",
      ),
    /verified reuse, not preparation in place/,
  );
});
