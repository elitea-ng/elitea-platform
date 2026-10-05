// Image-owned adapter. Execute only inside the resource-limited Code container.
// User modules have the process's Deno permissions; this is not a JS sandbox.
import { verifyJavaScriptExecution } from "./javascript_hydration_job.mjs";
import {preparedCapabilities,imageClient,writePlatformResult} from "./platform_prepared.mjs";
const encoder = new TextEncoder();

export async function executeJavaScript(source, input, language, scratch, platform = null) {
  if (!["javascript", "typescript"].includes(language)) {
    throw new Error("Unsupported JavaScript runtime language");
  }
  const directory = await Deno.makeTempDir({ dir: scratch, prefix: "code-" });
  const originalLog = console.log;
  const originalInfo = console.info;
  const previousState = globalThis.elitea_state;
  const previousLegacy = globalThis.alita_state;
  const hadClient=Object.hasOwn(globalThis,"elitea_client"),hadLegacyClient=Object.hasOwn(globalThis,"alita_client");
  const previousClient=globalThis.elitea_client,previousLegacyClient=globalThis.alita_client;
  try {
    // Never embed state in source or accept a caller-supplied module path.
    globalThis.elitea_state = structuredClone(input);
    globalThis.alita_state = { ...globalThis.elitea_state };
    if (platform) {globalThis.elitea_client=platform;globalThis.alita_client=platform;}
    console.log = (...args) => console.error(...args);
    console.info = (...args) => console.error(...args);
    const path = directory +
      (language === "typescript" ? "/user-code.ts" : "/user-code.mjs");
    await Deno.writeTextFile(path, source);
    const module = await import(
      "file://" + path.split("/").map(encodeURIComponent).join("/")
    );
    if (!Object.hasOwn(module, "default")) {
      throw new Error(
        "Code must export a default result or an async function returning a result",
      );
    }
    const result = await (typeof module.default === "function"
      ? module.default(globalThis.elitea_state)
      : module.default);
    const serialized = JSON.stringify(result, (_key, value) => {
      if (
        ["undefined", "function", "symbol", "bigint"].includes(typeof value) ||
        (typeof value === "number" && !Number.isFinite(value))
      ) {
        throw new Error("Code result must contain only finite JSON values");
      }
      return value;
    });
    if (encoder.encode(serialized).length > 256 * 1024) {
      throw new Error(
        "Code result exceeds the 256 KiB structured result limit",
      );
    }
    return JSON.parse(serialized);
  } finally {
    console.log = originalLog;
    console.info = originalInfo;
    globalThis.elitea_state = previousState;
    globalThis.alita_state = previousLegacy;
    if (hadClient) globalThis.elitea_client=previousClient; else delete globalThis.elitea_client;
    if (hadLegacyClient) globalThis.alita_client=previousLegacyClient; else delete globalThis.alita_client;
    await Deno.remove(directory, { recursive: true });
  }
}

if (import.meta.main) {
  const [path, scratch] = Deno.args;
  if (!path || !scratch || (await Deno.stat(path)).size > 1024 * 1024) {
    throw new Error("Code request or scratch configuration is invalid");
  }
  const request = JSON.parse(await Deno.readTextFile(path));
  const capability=preparedCapabilities(request);
  if (
    ![1, 3].includes(capability.baseRevision) || typeof request.source !== "string" ||
    encoder.encode(request.source).length > 256 * 1024 ||
    !request.input || Array.isArray(request.input) ||
    typeof request.input !== "object"
  ) {
    throw new Error("Code request is invalid for the JavaScript runtime");
  }
  if (capability.baseRevision === 3) await verifyJavaScriptExecution();
  else if (
    Object.hasOwn(request, "native_dependencies") ||
    Object.hasOwn(request, "dependency_bundle_sha256")
  ) throw new Error("Invalid legacy dependency request");
  const result = await executeJavaScript(
    request.source,
    request.input,
    request.language,
    scratch,
    imageClient(capability.broker),
  );
  if (capability.broker) await writePlatformResult(result);
  else console.log(JSON.stringify({ revision: 1, result }));
}
