// Image-owned adapter. Execute only inside the resource-limited Code container.
// User modules have the process's Deno permissions; this is not a JS sandbox.
const encoder = new TextEncoder();

export async function executeJavaScript(source, input, language, scratch) {
  if (!["javascript", "typescript"].includes(language)) {
    throw new Error("Unsupported JavaScript runtime language");
  }
  const directory = await Deno.makeTempDir({ dir: scratch, prefix: "code-" });
  const originalLog = console.log;
  const originalInfo = console.info;
  const previousState = globalThis.elitea_state;
  const previousLegacy = globalThis.alita_state;
  try {
    // Never embed state in source or accept a caller-supplied module path.
    globalThis.elitea_state = structuredClone(input);
    globalThis.alita_state = { ...globalThis.elitea_state };
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
    await Deno.remove(directory, { recursive: true });
  }
}

if (import.meta.main) {
  const [path, scratch] = Deno.args;
  if (!path || !scratch || (await Deno.stat(path)).size > 1024 * 1024) {
    throw new Error("Code request or scratch configuration is invalid");
  }
  const request = JSON.parse(await Deno.readTextFile(path));
  if (
    request.revision !== 1 || typeof request.source !== "string" ||
    encoder.encode(request.source).length > 256 * 1024 ||
    !request.input || Array.isArray(request.input) ||
    typeof request.input !== "object"
  ) {
    throw new Error("Code request is invalid for the JavaScript runtime");
  }
  const result = await executeJavaScript(
    request.source,
    request.input,
    request.language,
    scratch,
  );
  console.log(JSON.stringify({ revision: 1, result }));
}
