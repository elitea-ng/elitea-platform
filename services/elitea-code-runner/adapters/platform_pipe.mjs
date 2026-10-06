// Only the broker-enabled native launch contract supplies these retained child pipes.
import {decodeFrame, LIMITS, PlatformError} from "./platform_client.mjs";
const invalid = () => { throw new PlatformError("observation_unknown"); };
async function readExact(reader, length) {
  const output = new Uint8Array(length); let offset = 0;
  while (offset < length) {
    const count = await reader.read(output.subarray(offset));
    if (count === null || count < 1 || count > length - offset) invalid();
    offset += count;
  }
  return output;
}
async function writeExact(writer, bytes) {
  let offset = 0;
  while (offset < bytes.length) {
    const count = await writer.write(bytes.subarray(offset));
    if (count < 1 || count > bytes.length - offset) invalid();
    offset += count;
  }
}
export function retainedPipeExchange(reader, writer) {
  let busy = false, unknown = false;
  return async (request) => {
    if (busy || unknown) invalid();
    const [header] = decodeFrame(request);
    busy = true;
    try {
      await writeExact(writer, request);
      const prefix = await readExact(reader, 8);
      const lengths = new DataView(prefix.buffer);
      const head = lengths.getUint32(0, false), body = lengths.getUint32(4, false);
      if (head > LIMITS.reply || body > LIMITS.chunk) invalid();
      const reply = new Uint8Array(8 + head + body);reply.set(prefix);reply.set(await readExact(reader, head + body), 8);
      const [response] = decodeFrame(reply, true);
      if (response.sequence !== header.sequence) invalid();
      return reply;
    } catch (error) { unknown = true; throw error; }
    finally { busy = false; }
  };
}
