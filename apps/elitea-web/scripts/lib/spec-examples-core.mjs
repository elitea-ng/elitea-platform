/**
 * Decision logic for scripts/check-spec-examples.mjs: every `example` and
 * `examples` value in an OpenAPI 3.0 document must satisfy the schema it
 * sits beside.
 *
 * Why it matters here: orval generates the web test mocks with
 * `useExamples: true`, so a response example IS the body a test receives.
 * Nothing else reads an example against its schema — oapi-codegen and orval
 * both copy it through verbatim — so without this check a wrong example
 * would feed tests a shape the server never sends.
 *
 * Where examples are looked for (the places OpenAPI 3.0 allows them):
 *   - media types (request bodies and responses, inline or under
 *     components), checked against the media type's schema;
 *   - parameters and headers, checked against their schema;
 *   - any schema node (`example` on a schema or a property), checked against
 *     that node.
 * An `examples` entry may be a `$ref` to `#/components/examples/*`; its
 * `value` is what is checked (an `externalValue` has nothing local to check).
 *
 * OpenAPI 3.0 schemas are not JSON Schema: `nullable: true` is translated to
 * `anyOf: [<schema>, {type: null}]`, and the OpenAPI-only keywords
 * (`example`, `discriminator`, `xml`, …) are left for Ajv to ignore
 * (`strict: false`). `int32`/`int64` are range-checked, `binary`/`byte` are
 * accepted as any string, the rest come from ajv-formats.
 */
import Ajv from 'ajv';
import addFormats from 'ajv-formats';

const SPEC_ID = 'spec';
const HTTP_METHODS = ['get', 'put', 'post', 'delete', 'options', 'head', 'patch', 'trace'];
const INT32 = 2 ** 31;

/** `/a/b~c` style JSON pointer segment escaping (RFC 6901). */
function seg(key) {
  return String(key).replaceAll('~', '~0').replaceAll('/', '~1');
}

/** Resolves a local `#/...` reference, or returns the node itself. */
function deref(spec, node) {
  if (node === null || typeof node !== 'object' || typeof node.$ref !== 'string') return node;
  const path = node.$ref.replace(/^#\//, '').split('/');
  let cur = spec;
  for (const raw of path) {
    cur = cur?.[raw.replaceAll('~1', '/').replaceAll('~0', '~')];
  }
  return cur;
}

/**
 * OpenAPI 3.0 schema → JSON Schema (draft-07, Ajv's default). Local refs are
 * re-pointed at the whole spec, registered under SPEC_ID, so an inline schema
 * can reach `#/components/schemas/*`.
 */
export function toJsonSchema(node) {
  if (Array.isArray(node)) return node.map(toJsonSchema);
  if (node === null || typeof node !== 'object') return node;
  const out = {};
  for (const [key, value] of Object.entries(node)) {
    if (key === 'nullable') continue;
    if (key === '$ref' && typeof value === 'string' && value.startsWith('#')) {
      out.$ref = `${SPEC_ID}${value}`;
      continue;
    }
    out[key] = toJsonSchema(value);
  }
  // 3.0's boolean exclusiveMinimum/Maximum modify minimum/maximum; draft-07's
  // are the bound itself.
  for (const [exclusive, bound] of [['exclusiveMinimum', 'minimum'], ['exclusiveMaximum', 'maximum']]) {
    if (typeof out[exclusive] !== 'boolean') continue;
    if (out[exclusive]) {
      out[exclusive] = out[bound];
      delete out[bound];
    } else {
      delete out[exclusive];
    }
  }
  return node.nullable === true ? { anyOf: [out, { type: 'null' }] } : out;
}

/** Every value an `examples` map carries, with its pointer. */
function exampleEntries(spec, examples, pointer) {
  const entries = [];
  for (const [name, entry] of Object.entries(examples)) {
    const resolved = deref(spec, entry);
    if (resolved !== null && typeof resolved === 'object' && 'value' in resolved) {
      entries.push({ pointer: `${pointer}/examples/${seg(name)}`, value: resolved.value });
    }
  }
  return entries;
}

/**
 * Walks every place an example can live and returns
 * `{ pointer, schema, value }` cases. `schema` is the raw OpenAPI schema.
 */
export function collectExampleCases(spec) {
  const cases = [];
  const seenSchemas = new Set();

  const walkSchema = (schema, pointer) => {
    if (schema === null || typeof schema !== 'object' || '$ref' in schema || seenSchemas.has(schema)) return;
    seenSchemas.add(schema);
    if ('example' in schema) cases.push({ pointer: `${pointer}/example`, schema, value: schema.example });
    for (const [name, prop] of Object.entries(schema.properties ?? {})) {
      walkSchema(prop, `${pointer}/properties/${seg(name)}`);
    }
    walkSchema(schema.items, `${pointer}/items`);
    walkSchema(schema.additionalProperties, `${pointer}/additionalProperties`);
    walkSchema(schema.not, `${pointer}/not`);
    for (const key of ['allOf', 'anyOf', 'oneOf']) {
      (schema[key] ?? []).forEach((sub, i) => walkSchema(sub, `${pointer}/${key}/${i}`));
    }
  };

  // A media type, parameter or header: example/examples against `.schema`.
  const walkCarrier = (carrierRaw, pointer) => {
    const carrier = deref(spec, carrierRaw);
    if (carrier === null || typeof carrier !== 'object' || carrier !== carrierRaw) return;
    const schema = carrier.schema;
    walkSchema(schema, `${pointer}/schema`);
    if (schema === undefined) return;
    if ('example' in carrier) cases.push({ pointer: `${pointer}/example`, schema, value: carrier.example });
    for (const entry of exampleEntries(spec, carrier.examples ?? {}, pointer)) cases.push({ ...entry, schema });
  };

  const walkContent = (content, pointer) => {
    for (const [mediaType, media] of Object.entries(content ?? {})) {
      walkCarrier(media, `${pointer}/content/${seg(mediaType)}`);
    }
  };

  const walkResponse = (response, pointer) => {
    if (response === null || typeof response !== 'object' || '$ref' in response) return;
    walkContent(response.content, pointer);
    for (const [name, header] of Object.entries(response.headers ?? {})) {
      walkCarrier(header, `${pointer}/headers/${seg(name)}`);
    }
  };

  const walkBody = (body, pointer) => {
    if (body === null || typeof body !== 'object' || '$ref' in body) return;
    walkContent(body.content, pointer);
  };

  for (const [path, item] of Object.entries(spec.paths ?? {})) {
    const base = `#/paths/${seg(path)}`;
    (item.parameters ?? []).forEach((p, i) => walkCarrier(p, `${base}/parameters/${i}`));
    for (const method of HTTP_METHODS) {
      const op = item[method];
      if (op === undefined) continue;
      const opBase = `${base}/${method}`;
      (op.parameters ?? []).forEach((p, i) => walkCarrier(p, `${opBase}/parameters/${i}`));
      walkBody(op.requestBody, `${opBase}/requestBody`);
      for (const [status, response] of Object.entries(op.responses ?? {})) {
        walkResponse(response, `${opBase}/responses/${seg(status)}`);
      }
    }
  }

  const components = spec.components ?? {};
  for (const [name, schema] of Object.entries(components.schemas ?? {})) {
    walkSchema(schema, `#/components/schemas/${seg(name)}`);
  }
  for (const [name, p] of Object.entries(components.parameters ?? {})) {
    walkCarrier(p, `#/components/parameters/${seg(name)}`);
  }
  for (const [name, h] of Object.entries(components.headers ?? {})) {
    walkCarrier(h, `#/components/headers/${seg(name)}`);
  }
  for (const [name, r] of Object.entries(components.responses ?? {})) {
    walkResponse(r, `#/components/responses/${seg(name)}`);
  }
  for (const [name, b] of Object.entries(components.requestBodies ?? {})) {
    walkBody(b, `#/components/requestBodies/${seg(name)}`);
  }
  return cases;
}

function makeAjv(spec) {
  const ajv = new Ajv({ strict: false, allErrors: true, validateSchema: false });
  addFormats(ajv);
  ajv.addFormat('int32', { type: 'number', validate: (n) => Number.isInteger(n) && n >= -INT32 && n < INT32 });
  ajv.addFormat('int64', { type: 'number', validate: (n) => Number.isInteger(n) });
  ajv.addFormat('binary', true);
  ajv.addFormat('byte', true);
  ajv.addSchema({ $id: SPEC_ID, components: toJsonSchema(spec.components ?? {}) });
  return ajv;
}

/**
 * Validates every example in `spec`. Returns `{ checked, findings }`; each
 * finding is `{ pointer, message }`. Empty findings == pass.
 */
export function checkSpecExamples(spec) {
  const ajv = makeAjv(spec);
  const cases = collectExampleCases(spec);
  const findings = [];
  for (const { pointer, schema, value } of cases) {
    const validate = ajv.compile(toJsonSchema(schema));
    if (!validate(value)) {
      const detail = validate.errors.map((e) => `${e.instancePath || '(root)'} ${e.message}`).join('; ');
      findings.push({ pointer, message: `example does not satisfy its schema: ${detail}` });
    }
  }
  return { checked: cases.length, findings };
}
