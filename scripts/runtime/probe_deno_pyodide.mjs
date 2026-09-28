// Compatibility probe only. Production execution still requires an outer sandbox.
import { loadPyodide } from 'npm:pyodide@0.29.0';

const packageCacheDir = Deno.args[0];
if (!packageCacheDir) throw new Error('Supply a separate writable wheel-cache directory');
const python = await loadPyodide({ packageCacheDir });
await python.loadPackage('micropip');

const result = JSON.parse(await python.runPythonAsync(`
import micropip
await micropip.install('idna==3.10')
import idna, json
elitea_state = {'domain': 'bücher.example'}
json.dumps({'domain': idna.encode(elitea_state['domain']).decode(), 'version': idna.__version__})
`));
if (result.domain !== 'xn--bcher-kva.example' || result.version !== '3.10') {
  throw new Error('Package installation or Python execution returned an incorrect result');
}

// Preserve literal backslashes. The legacy runner previously corrupted these.
const escaped = await python.runPythonAsync(String.raw`json.dumps({'text': 'one\ntwo'})`);
if (JSON.parse(escaped).text !== 'one\ntwo') throw new Error('Python source escaping changed');

// This checks the interpreter boundary, not OS stdin transport or worker limits.
const source = `payload = ${JSON.stringify('x'.repeat(160 * 1024))}\nlen(payload)`;
if (await python.runPythonAsync(source) !== 160 * 1024) {
  throw new Error('Large Python source was truncated');
}

let failed = false;
try {
  await python.runPythonAsync('raise ValueError("code-probe-failure")');
} catch (error) {
  failed = String(error).includes('code-probe-failure');
}
if (!failed) throw new Error('Python exception did not reach the caller');

console.log(JSON.stringify({
  deno: Deno.version.deno,
  pyodide: python.version,
  package: result,
  checks: ['top-level await', 'micropip', 'escaping', '160 KiB source', 'exception propagation'],
}));
