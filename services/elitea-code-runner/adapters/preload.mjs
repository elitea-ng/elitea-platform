// Build-time only. Runtime jobs do not download the interpreter or base wheels.
import { loadPyodide } from "npm:pyodide@0.29.0";
const python = await loadPyodide({ packageCacheDir: "/opt/elitea-wheels" });
await python.loadPackage("micropip");
