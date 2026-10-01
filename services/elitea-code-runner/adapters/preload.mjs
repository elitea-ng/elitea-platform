// Trusted image and on-demand preparation. Never execute user source here.
import { loadPyodide } from "npm:pyodide@0.29.0";
import { materializeWheels } from "./python_wheels.mjs";

export async function preparePythonPackages(
  packageCacheDir,
  requirements = [],
  { requireExactVersions = true, interpreter } = {},
) {
  if (
    !Array.isArray(requirements) || requirements.length > 128 ||
    requirements.some((entry) =>
      typeof entry !== "string" || entry.length > 256
    )
  ) {
    throw new Error(
      "Python package profile must contain at most 128 bounded requirements",
    );
  }
  await Deno.mkdir(packageCacheDir, { recursive: true });
  const python = interpreter ?? await loadPyodide({ packageCacheDir });
  await python.loadPackage(["micropip", "packaging"]);
  python.globals.set("_elitea_requirements_json", JSON.stringify(requirements));
  python.globals.set("_elitea_require_exact_versions", requireExactVersions);
  const frozen = await python.runPythonAsync(`
import json
import micropip
from packaging.requirements import Requirement
requirements = json.loads(_elitea_requirements_json)
for raw in requirements:
    requirement = Requirement(raw)
    pins = list(requirement.specifier)
    if requirement.url or requirement.marker:
        raise ValueError('Package preparation requires registry names without URLs or markers')
    if (_elitea_require_exact_versions and (len(pins) != 1
        or pins[0].operator != '==' or '*' in pins[0].version)):
        raise ValueError('Package profiles require exact versions without URLs or markers')
await micropip.install(requirements)
micropip.freeze()
`);
  // Reuse the native lock format, including resolved transitive dependencies.
  // Loading through a fresh interpreter populates Pyodide's wheel cache, unlike
  // micropip installation alone, which only populates its virtual filesystem.
  const lockPath = `${packageCacheDir}/elitea-python-lock.json`;
  const lock = JSON.parse(frozen);
  const packages = JSON.parse(
    python.runPython("json.dumps(list(micropip.list()))"),
  );
  // freeze() also retains the complete upstream catalogue. Only the installed
  // closure belongs to this approved offline profile.
  lock.packages = Object.fromEntries(
    packages.map((name) => [name, lock.packages[name]]),
  );
  await materializeWheels(lock, packageCacheDir);
  await Deno.writeTextFile(lockPath, JSON.stringify(lock));
  const cached = await loadPyodide({ packageCacheDir, lockFileURL: lockPath });
  await cached.loadPackage(packages);
  if (packages.some((name) => !Object.hasOwn(cached.loadedPackages, name))) {
    throw new Error("Prepared Python package set is incomplete");
  }
  return lock;
}

if (import.meta.main) {
  const [cache = "/opt/elitea-wheels", profile] = Deno.args;
  const requirements = profile
    ? JSON.parse(await Deno.readTextFile(profile))
    : [];
  await preparePythonPackages(cache, requirements);
}
