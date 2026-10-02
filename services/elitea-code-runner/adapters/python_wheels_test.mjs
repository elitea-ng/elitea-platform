import { materializeWheels, wheelSource } from "./python_wheels.mjs";

const digest =
  "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824";
const wheel = () => ({
  file_name: "https://files.pythonhosted.org/packages/demo-1-py3-none-any.whl",
  sha256: digest,
});

Deno.test("wheel sources reject alternate authority and unsafe filenames", () => {
  for (
    const url of [
      "http://files.pythonhosted.org/packages/a.whl",
      "https://example.com/a.whl",
      "https://files.pythonhosted.org:444/a.whl",
      "https://user@files.pythonhosted.org/a.whl",
      "https://files.pythonhosted.org/a.whl?token=x",
      "https://files.pythonhosted.org/%2e%2e.whl",
      "https://files.pythonhosted.org/a.zip",
    ]
  ) {
    let rejected = false;
    try {
      wheelSource({ ...wheel(), file_name: url });
    } catch {
      rejected = true;
    }
    if (!rejected) throw new Error(`Unexpected wheel source admitted: ${url}`);
  }
});

Deno.test("cached wheels are reused offline and tampering is rejected", async () => {
  const directory = await Deno.makeTempDir();
  try {
    const path = `${directory}/demo-1-py3-none-any.whl`;
    await Deno.writeTextFile(path, "hello");
    const lock = { packages: { demo: wheel() } };
    await materializeWheels(lock, directory);
    if (lock.packages.demo.file_name !== "demo-1-py3-none-any.whl") {
      throw new Error("Wheel path was not normalized");
    }
    const localLock = { packages: { demo: { ...wheel(), file_name: path } } };
    await materializeWheels(localLock, directory);
    if (localLock.packages.demo.file_name !== "demo-1-py3-none-any.whl") {
      throw new Error("Build-time path leaked into the portable lock");
    }
    await Deno.writeTextFile(path, "changed");
    let rejected = false;
    try {
      await materializeWheels({ packages: { demo: wheel() } }, directory);
    } catch (error) {
      rejected = String(error).includes("digest does not match");
    }
    if (!rejected) throw new Error("Tampered cache was accepted");
  } finally {
    await Deno.remove(directory, { recursive: true });
  }
});
