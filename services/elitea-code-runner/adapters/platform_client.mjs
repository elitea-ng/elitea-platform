// Credential-free JS/TS client. The image owns the byte exchange function.
export const LIMITS = Object.freeze({ request: 262144, reply: 2097152, chunk: 65536,
  object: 8388608, depth: 32, values: 10000, calls: 4096 });
const encoder = new TextEncoder();
const decoder = new TextDecoder("utf-8", { fatal: true });
const operations = new Set(["application_list", "application_get", "application_version_get", "user_get",
  "toolkit_list", "toolkit_call", "secret_read", "bucket_exists", "bucket_create",
  "artifact_list", "artifact_head", "artifact_read", "artifact_read_chunk",
  "artifact_write_begin", "artifact_write_chunk", "artifact_write_commit", "artifact_append", "artifact_delete"]);
const statuses = new Set(["ok", "not_found", "sharing_denied", "authorization_denied", "authentication_denied",
  "approval_required", "sensitive_rejected", "dependency_unavailable", "unsupported_operation", "invalid_resource", "invalid_frame",
  "resource_exhausted", "revision_conflict", "unknown_effect", "stopped", "lease_lost"]);
const object = (v) => v !== null && typeof v === "object" && !Array.isArray(v);
const exact = (v, keys) => object(v) && Object.keys(v).length === keys.length && keys.every((k) => Object.hasOwn(v, k));
const digest = (v) => typeof v === "string" && /^[a-f0-9]{64}$/.test(v);
export class PlatformError extends Error {
  constructor(code) { super("Code platform operation failed: " + code); this.code = code; }
}
const fail = (code = "invalid_frame") => { throw new PlatformError(code); };

// Parse before materializing objects. Duplicate keys, depth, and value count are bounded.
export function strictJSON(text) {
  let index = 0, values = 0;
  const white = () => { while (/[\x20\t\r\n]/.test(text[index] ?? "!")) index++; };
  function string() {
    const start = index++;
    let escaped = false;
    while (index < text.length) {
      const current = text[index++];
      if (!escaped && current === '"') {
        try { return JSON.parse(text.slice(start, index)); } catch { fail(); }
      }
      if (!escaped && current.charCodeAt(0) < 32) fail();
      escaped = !escaped && current === "\\";
    }
    fail();
  }
  function value(depth) {
    if (depth > LIMITS.depth || ++values > LIMITS.values) fail("resource_exhausted");
    white();
    const current = text[index];
    if (current === '"') return string();
    if (current === "{" || current === "[") {
      const isObject = current === "{";
      const end = isObject ? "}" : "]";
      const result = isObject ? Object.create(null) : [];
      index++; white();
      if (text[index] === end) { index++; return result; }
      for (;;) {
        if (isObject) {
          if (text[index] !== '"') fail();
          const key = string();
          if (Object.hasOwn(result, key)) fail();
          white(); if (text[index++] !== ":") fail();
          result[key] = value(depth + 1);
        } else result.push(value(depth + 1));
        white();
        const next = text[index++];
        if (next === end) return result;
        if (next !== ",") fail();
        white();
      }
    }
    for (const [literal, result] of [["null", null], ["true", true], ["false", false]]) {
      if (text.startsWith(literal, index)) { index += literal.length; return result; }
    }
    const number = text.slice(index).match(/^-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?/);
    if (!number) fail();
    index += number[0].length;
    const result = Number(number[0]);
    if (!Number.isFinite(result)) fail();
    return result;
  }
  const result = value(0); white();
  if (index !== text.length) fail();
  return result;
}

function validateHeader(header, reply) {
  const keys = reply ? ["revision", "sequence", "status", "result", "receipt"] : ["revision", "sequence", "operation", "resource", "arguments"];
  if (!exact(header, keys) || header.revision !== 1 || !Number.isInteger(header.sequence) || header.sequence < 1 || header.sequence > LIMITS.calls) fail();
  if (!reply) {
    if (!operations.has(header.operation) || !object(header.resource) || !object(header.arguments)) fail();
    return;
  }
  if (!statuses.has(header.status)) fail("invalid_reply");
  const receipt = header.receipt;
  if (receipt !== null && (!exact(receipt, ["effect_id", "call_sha256", "state"]) ||
    !digest(receipt.effect_id) || !digest(receipt.call_sha256) || !["committed", "uncertain"].includes(receipt.state))) fail("invalid_reply");
  if (header.status === "ok" && (receipt === null || receipt.state !== "committed")) fail("invalid_reply");
  if (header.status !== "ok" && header.result !== null) fail("invalid_reply");
}

// Serialize only plain data. No toJSON, accessors, or unbounded string allocation.
export function boundedJSON(value, bound) {
  const buffer = new Uint8Array(bound);
  let offset = 0, values = 0;
  const byte = (v) => { if (offset === bound) fail("resource_exhausted"); buffer[offset++] = v; };
  const ascii = (text) => { for (let i = 0; i < text.length; i++) byte(text.charCodeAt(i)); };
  function string(text) {
    if (text.length > bound - offset) fail("resource_exhausted");
    byte(34);
    for (let i = 0; i < text.length; i++) {
      let c = text.charCodeAt(i);
      if (c === 34 || c === 92) { byte(92); byte(c); }
      else if (c < 32) ascii("\\u" + c.toString(16).padStart(4, "0"));
      else {
        if (c >= 0xd800 && c <= 0xdbff) {
          const low = text.charCodeAt(++i);
          if (!(low >= 0xdc00 && low <= 0xdfff)) fail();
          c = 0x10000 + ((c - 0xd800) << 10) + low - 0xdc00;
        } else if (c >= 0xdc00 && c <= 0xdfff) fail();
        if (c < 128) byte(c);
        else if (c < 2048) { byte(192 | (c >> 6)); byte(128 | (c & 63)); }
        else if (c < 65536) { byte(224 | (c >> 12)); byte(128 | ((c >> 6) & 63)); byte(128 | (c & 63)); }
        else { byte(240 | (c >> 18)); byte(128 | ((c >> 12) & 63)); byte(128 | ((c >> 6) & 63)); byte(128 | (c & 63)); }
      }
    }
    byte(34);
  }
  function write(item, depth) {
    if (depth > LIMITS.depth || ++values > LIMITS.values) fail("resource_exhausted");
    if (item === null) { ascii("null"); return; }
    switch (typeof item) {
      case "string": string(item); return;
      case "boolean": ascii(item ? "true" : "false"); return;
      case "number": if (!Number.isFinite(item)) fail(); ascii(String(item)); return;
      case "object": break;
      default: fail();
    }
    const array = Array.isArray(item);
    if (array) {
      if (item.length > LIMITS.values - values) fail("resource_exhausted");
      byte(91);
      for (let i = 0; i < item.length; i++) {
        if (i) byte(44);
        const descriptor = Object.getOwnPropertyDescriptor(item, String(i));
        if (!descriptor || !Object.hasOwn(descriptor, "value")) fail();
        write(descriptor.value, depth + 1);
      }
      byte(93); return;
    }
    const prototype = Object.getPrototypeOf(item);
    if (prototype !== Object.prototype && prototype !== null) fail();
    byte(123); let first = true;
    for (const key in item) {
      if (!Object.hasOwn(item, key)) continue;
      const descriptor = Object.getOwnPropertyDescriptor(item, key);
      if (!descriptor || !Object.hasOwn(descriptor, "value")) fail();
      if (!first) byte(44); first = false;
      string(key); byte(58); write(descriptor.value, depth + 1);
    }
    byte(125);
  }
  write(value, 0);
  return buffer.subarray(0, offset);
}
export function encodeFrame(header, payload = new Uint8Array(), reply = false) {
  if (!(payload instanceof Uint8Array) || payload.length > LIMITS.chunk) fail("resource_exhausted");
  const bytes = boundedJSON(header, reply ? LIMITS.reply : LIMITS.request);
  // Check the bounded plain copy, without invoking caller accessors.
  validateHeader(strictJSON(decoder.decode(bytes)), reply);
  const frame = new Uint8Array(8 + bytes.length + payload.length);
  new DataView(frame.buffer).setUint32(0, bytes.length, false);
  new DataView(frame.buffer).setUint32(4, payload.length, false);
  frame.set(bytes, 8); frame.set(payload, 8 + bytes.length);
  return frame;
}

export function decodeFrame(frame, reply = false) {
  const max = reply ? LIMITS.reply : LIMITS.request;
  if (!(frame instanceof Uint8Array) || frame.length < 8 || frame.length > 8 + max + LIMITS.chunk) fail();
  const view = new DataView(frame.buffer, frame.byteOffset, frame.byteLength);
  const head = view.getUint32(0, false), body = view.getUint32(4, false);
  if (head > max || body > LIMITS.chunk || frame.length !== 8 + head + body) fail();
  let header;
  try { header = strictJSON(decoder.decode(frame.subarray(8, 8 + head))); } catch { fail(); }
  validateHeader(header, reply);
  if (reply && header.status !== "ok" && body) fail("invalid_reply");
  return [header, frame.slice(8 + head)];
}

const id = (v) => {
  const text = String(v);
  if (!/^[1-9][0-9]{0,9}$/.test(text) || Number(text) > 2147483647) fail("invalid_resource");
  return text;
};
const unset = Symbol("unset");

export class SandboxClient {
  #exchange; #max; #sequence = 0; #busy = false; #unknown = false;
  constructor(exchange, maxCalls = 128) {
    if (typeof exchange !== "function" || !Number.isInteger(maxCalls) || maxCalls < 1 || maxCalls > LIMITS.calls) fail("invalid_binding");
    this.#exchange = exchange; this.#max = maxCalls;
  }
  async call(operation, resource, arguments_ = {}, payload = new Uint8Array()) {
    if (this.#unknown) fail("unknown_effect");
    if (!operations.has(operation)) fail("invalid_resource");
    if (this.#busy) fail("in_flight_limit");
    if (this.#sequence >= this.#max) fail("call_limit");
    const sequence = this.#sequence + 1;
    const frame = encodeFrame({ revision: 1, sequence, operation, resource, arguments: arguments_ }, payload);
    this.#sequence = sequence; this.#busy = true;
    try {
      let header, body;
      try {
        [header, body] = decodeFrame(await this.#exchange(frame), true);
        if (header.sequence !== sequence) fail("invalid_reply");
      } catch { this.#unknown = true; fail("observation_unknown"); }
      if (header.status !== "ok") {
        if (["unknown_effect", "stopped", "lease_lost"].includes(header.status)) this.#unknown = true;
        fail(header.status);
      }
      return [header.result, body];
    } finally { this.#busy = false; }
  }
  async #result(op, resource, args = {}) { return (await this.call(op, resource, args))[0]; }
  get_user_data() { return this.#result("user_get", { kind: "current_user" }); }
  get_list_of_apps(cursor = null, limit = 100) { return this.#result("application_list", { kind: "application_catalog" }, { cursor, limit }); }
  get_app_details(app) { return this.#result("application_get", { kind: "application", id: id(app) }); }
  get_app_version_details(app, version) { return this.#result("application_version_get", { kind: "application_version", application_id: id(app), version_id: id(version) }); }
  get_mcp_toolkits(cursor = null, limit = 100) { return this.#result("toolkit_list", { kind: "toolkit_catalog" }, { cursor, limit }); }
  mcp_tool_call(toolkit, revision, tool, args) {
    if (!digest(revision)) fail("invalid_resource");
    return this.#result("toolkit_call", { kind: "toolkit", id: id(toolkit), revision, tool }, args);
  }
  unsecret(name) { return this.#result("secret_read", { kind: "secret", scope: "project", name }); }
  async get_private_project_secret(name, defaultValue = unset) {
    try { return await this.#result("secret_read", { kind: "secret", scope: "personal", name }); }
    catch (error) { if (error instanceof PlatformError && error.code === "not_found" && defaultValue !== unset) return defaultValue; throw error; }
  }
  bucket_exists(name) { return this.#result("bucket_exists", { kind: "bucket", name }); }
  create_bucket(name) { return this.#result("bucket_create", { kind: "bucket", name }); }
  invalidObservation() { this.#unknown = true; fail("unknown_effect"); }
  artifact(bucket) { return new SandboxArtifact(this, bucket); }
}

export class SandboxArtifact {
  #client; #bucket;
  constructor(client, bucket) { this.#client = client; this.#bucket = bucket; }
  #resource(name) { return { kind: "artifact", bucket: this.#bucket, name }; }
  async list(prefix = "", cursor = null, limit = 100) { return (await this.#client.call("artifact_list", { kind: "bucket", name: this.#bucket }, { prefix, cursor, limit }))[0]; }
  async head(name) { return (await this.#client.call("artifact_head", this.#resource(name)))[0]; }
  async get_content_bytes(name) {
    const [info, first] = await this.#client.call("artifact_read", this.#resource(name));
    if (!exact(info, ["bytes", "transfer", "version"]) || !Number.isInteger(info.bytes) || info.bytes < 0 || info.bytes > LIMITS.object || !digest(info.transfer) || typeof info.version !== "string" || info.version.length < 1 || info.version.length > 1024 || /[\x00-\x1f\x7f]/.test(info.version)) this.#client.invalidObservation();
    const result = new Uint8Array(info.bytes);
    if (first.length > result.length) this.#client.invalidObservation();
    result.set(first); let offset = first.length;
    while (offset < result.length) {
      const [, chunk] = await this.#client.call("artifact_read_chunk", { kind: "artifact_transfer", id: info.transfer }, { offset });
      if (chunk.length === 0 || chunk.length > result.length - offset) this.#client.invalidObservation();
      result.set(chunk, offset); offset += chunk.length;
    }
    return result;
  }
  async create(name, content) {
    if (typeof content === "string") {
      if (content.length > LIMITS.object) fail("resource_exhausted");
      let bytes = 0;
      for (const character of content) { const code = character.codePointAt(0); bytes += code < 128 ? 1 : code < 2048 ? 2 : code < 65536 ? 3 : 4; if (bytes > LIMITS.object) fail("resource_exhausted"); }
    }
    const data = typeof content === "string" ? encoder.encode(content) : content;
    if (!(data instanceof Uint8Array) || data.length > LIMITS.object) fail("resource_exhausted");
    const [info] = await this.#client.call("artifact_write_begin", this.#resource(name), { bytes: data.length });
    if (!exact(info, ["transfer"]) || !digest(info.transfer)) this.#client.invalidObservation();
    const transfer = { kind: "artifact_transfer", id: info.transfer };
    for (let offset = 0; offset < data.length; offset += LIMITS.chunk) await this.#client.call("artifact_write_chunk", transfer, { offset }, data.slice(offset, offset + LIMITS.chunk));
    return (await this.#client.call("artifact_write_commit", transfer))[0];
  }
  async append(name, text, expectedVersion) { return (await this.#client.call("artifact_append", this.#resource(name), { text, expected_version: expectedVersion }))[0]; }
  async delete(name, expectedVersion) { return (await this.#client.call("artifact_delete", this.#resource(name), { expected_version: expectedVersion }))[0]; }
}
