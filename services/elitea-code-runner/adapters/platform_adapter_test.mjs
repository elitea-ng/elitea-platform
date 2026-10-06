import { executeJavaScript } from "./javascript.mjs";
import { SandboxClient, decodeFrame, encodeFrame } from "./platform_client.mjs";

for (const language of ["javascript", "typescript"]) {
  Deno.test(`${language} awaits the injected client and restores prior globals`, async () => {
    const previous = { current: { kept: true }, legacy: { kept: "legacy" } };
    globalThis.elitea_client = previous.current;
    globalThis.alita_client = previous.legacy;
    let calls = 0;
    const client = new SandboxClient(async frame => {
      calls++;
      const [request] = decodeFrame(frame);
      if (request.operation !== "user_get") throw new Error("Unexpected platform operation");
      return encodeFrame({ revision: 1, sequence: request.sequence, status: "ok",
        result: { name: "fixture" }, receipt: { effect_id: "a".repeat(64),
          call_sha256: "b".repeat(64), state: "committed" } }, new Uint8Array(), true);
    }, 1);
    const source = language === "typescript"
      ? `interface User { name: string }; export default async () => {
          const user: User = await elitea_client.get_user_data();
          return { name: user.name, same: elitea_client === alita_client };
        };`
      : `export default async () => ({
          name: (await elitea_client.get_user_data()).name,
          same: elitea_client === alita_client,
        });`;
    try {
      const result = await executeJavaScript(source, {}, language, "/tmp", client);
      if (result.name !== "fixture" || result.same !== true || calls !== 1) {
        throw new Error("Platform async result changed");
      }
      if (globalThis.elitea_client !== previous.current || globalThis.alita_client !== previous.legacy) {
        throw new Error("Platform globals were not restored");
      }
    } finally {
      delete globalThis.elitea_client;
      delete globalThis.alita_client;
    }
  });

  Deno.test(`${language} restores the client after invalid result failure`, async () => {
    const previous = { kept: true };
    globalThis.elitea_client = previous;
    delete globalThis.alita_client;
    try {
      let failed = false;
      try {
        await executeJavaScript("export default 'x'.repeat(256 * 1024);", {}, language, "/tmp", {});
      } catch (error) {
        if (!String(error).includes("structured result limit")) throw error;
        failed = true;
      }
      if (!failed || globalThis.elitea_client !== previous || Object.hasOwn(globalThis, "alita_client")) {
        throw new Error("Invalid result did not restore platform globals");
      }
    } finally {
      delete globalThis.elitea_client;
    }
  });
}
