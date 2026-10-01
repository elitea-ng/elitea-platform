// Runs only inside the resource-limited Code container. Deno permissions alone
// are not the isolation boundary. Never give this process platform credentials.
import { loadPyodide } from "npm:pyodide@0.29.0";

const encoder = new TextEncoder();
const MAX_RESULT_BYTES = 256 * 1024;

export async function executePython(source, input, packageCacheDir) {
  const python = await loadPyodide({
    packageCacheDir,
    lockFileURL: `${packageCacheDir}/elitea-python-lock.json`,
    stdout: (text) => console.error(text),
    stderr: (text) => console.error(text),
  });
  // State crosses as JSON data, never interpolated executable Python source.
  python.globals.set("_elitea_input_json", JSON.stringify(input));
  python.globals.set("_elitea_source", source);
  await python.runPythonAsync(`
import json as _elitea_json
import importlib.util as _elitea_importlib
from pyodide.code import find_imports as _elitea_find_imports
elitea_state = _elitea_json.loads(_elitea_input_json)
alita_state = elitea_state.copy()
`);
  // Preserve automatic preparation, including frozen import-name mappings.
  await python.loadPackage("micropip", {
    messageCallback: (text) => console.error(text),
  });
  const automatic = python.runPython(
    "'micropip' not in _elitea_find_imports(_elitea_source)",
  );
  if (automatic) {
    // The frozen lock maps import names to distributions and their dependencies.
    await python.loadPackagesFromImports(source, {
      messageCallback: (text) => console.error(text),
    });
  }
  await python.runPythonAsync(`
import micropip as _elitea_micropip
_elitea_imports = _elitea_find_imports(_elitea_source)
# An explicit micropip import delegates dependency ordering/version selection
# to the code; do not pre-install its later imports before pinned installs run.
if 'micropip' not in _elitea_imports:
    for _elitea_module in _elitea_imports:
        if _elitea_importlib.find_spec(_elitea_module) is None:
            await _elitea_micropip.install(_elitea_module)
`);
  const value = await python.runPythonAsync(source);
  try {
    python.globals.set("_elitea_result", value === undefined ? null : value);
    const serialized = python.runPython(
      '_elitea_json.dumps(_elitea_result, allow_nan=False, separators=(",", ":"))',
    );
    if (encoder.encode(serialized).length > MAX_RESULT_BYTES) {
      throw new Error(
        "Code result exceeds the 256 KiB structured result limit",
      );
    }
    return JSON.parse(serialized);
  } finally {
    // Release the returned Python proxy; each job owns a fresh interpreter.
    value?.destroy?.();
  }
}

if (import.meta.main) {
  const [path, packageCacheDir] = Deno.args;
  if (!path || !packageCacheDir || (await Deno.stat(path)).size > 1024 * 1024) {
    throw new Error("Code request or package-cache configuration is invalid");
  }
  const request = JSON.parse(await Deno.readTextFile(path));
  if (
    request.revision !== 1 || request.language !== "python" ||
    typeof request.source !== "string" ||
    encoder.encode(request.source).length > 256 * 1024 ||
    !request.input || Array.isArray(request.input) ||
    typeof request.input !== "object"
  ) {
    throw new Error("Code request is invalid for the Python runtime");
  }
  const result = await executePython(
    request.source,
    request.input,
    packageCacheDir,
  );
  console.log(JSON.stringify({ revision: 1, result }));
}
