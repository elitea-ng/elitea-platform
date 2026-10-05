// Runs only inside the resource-limited Code container. Deno permissions alone
// are not the isolation boundary. Never give this process platform credentials.
import { loadPyodide } from "npm:pyodide@0.29.0";
import { verifyPythonCodePackages } from "./prepare_python_code.mjs";

import {preparedCapabilities,writePlatformResult} from "./platform_prepared.mjs";
import {retainedPipeExchange} from "./platform_pipe.mjs";
const encoder = new TextEncoder();
const MAX_RESULT_BYTES = 256 * 1024;

export async function executePython(source, input, packageCacheDir, platform = null, workspace = null) {
  const python = await loadPyodide({
    packageCacheDir,
    lockFileURL: `${packageCacheDir}/elitea-python-lock.json`,
    stdout: (text) => console.error(text),
    stderr: (text) => console.error(text),
  });
  if (workspace) {
    // The fixed native entrypoint already verified the exact manifest and RO
    // kernel mount. Pyodide sees only this repository through NodeFS.
    python.FS.mkdirTree("/workspace/repository");
    python.FS.mount(python.FS.filesystems.NODEFS, { root: "/workspace/repository" }, "/workspace/repository");
  }
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
  if (platform) {
    const exchange=retainedPipeExchange(Deno.stdin,Deno.stdout);
    const clientSource=await Deno.readTextFile("/opt/elitea-code/platform_client.py");
    if (encoder.encode(clientSource).length>128*1024) throw new Error("Code client image asset exceeds its bound");
    python.globals.set("_elitea_platform_module_source",clientSource);
    python.globals.set("_elitea_platform_max_calls",platform.max_calls);
    python.globals.set("_elitea_platform_exchange",async (frame)=>{
      const bytes=frame?.toJs ? frame.toJs() : frame;
      const reply=await exchange(new Uint8Array(bytes));
      return python.toPy(reply);
    });
    await python.runPythonAsync(`
import types as _elitea_types
import sys as _elitea_sys
from importlib.machinery import ModuleSpec as _elitea_platform_spec
_elitea_platform_module = _elitea_types.ModuleType("elitea_platform")
_elitea_platform_module.__spec__ = _elitea_platform_spec("elitea_platform", loader=None)
exec(compile(_elitea_platform_module_source, "<elitea-image-platform-client>", "exec"), _elitea_platform_module.__dict__)
_elitea_sys.modules["elitea_platform"] = _elitea_platform_module
async def _elitea_retained_exchange(frame):
    return bytes(await _elitea_platform_exchange(frame))
elitea_client = _elitea_platform_module.SandboxClient(_elitea_retained_exchange, _elitea_platform_max_calls)
alita_client = elitea_client
`);
  }
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

export async function executePythonRequest(request, packageCacheDir) {
  const capability=preparedCapabilities(request);
  const hasBundle = Object.hasOwn(request, "dependency_bundle_sha256");
  if (
    !((capability.baseRevision === 1 && !hasBundle) ||
      (capability.baseRevision === 2 && hasBundle &&
        typeof request.dependency_bundle_sha256 === "string" &&
        /^[a-f0-9]{64}$/.test(request.dependency_bundle_sha256) &&
        request.dependency_bundle_sha256.length === 64)) ||
    request.language !== "python" ||
    typeof request.source !== "string" ||
    encoder.encode(request.source).length > 256 * 1024 ||
    !request.input || Array.isArray(request.input) ||
    typeof request.input !== "object"
  ) {
    throw new Error("Code request is invalid for the Python runtime");
  }
  if (hasBundle) {
    await verifyPythonCodePackages(
      packageCacheDir,
      request.dependency_bundle_sha256,
    );
  }
  return await executePython(
    request.source,
    request.input,
    packageCacheDir,
    capability.broker,
    request.workspace ?? null,
  );
}

if (import.meta.main) {
  const [path, packageCacheDir] = Deno.args;
  if (!path || !packageCacheDir || (await Deno.stat(path)).size > 1024 * 1024) {
    throw new Error("Code request or package-cache configuration is invalid");
  }
  const request = JSON.parse(await Deno.readTextFile(path));
  const result = await executePythonRequest(request, packageCacheDir);
  if (preparedCapabilities(request).broker) await writePlatformResult(result);
  else console.log(JSON.stringify({ revision: 1, result }));
}
