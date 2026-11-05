// A small JSON Schema validator for the subset the tasks use, the
// expected-value checks for extraction tasks, and the doc rubric.
//
// Subset: type (string or array), properties, required,
// additionalProperties (boolean), items (one schema), enum, const,
// minimum, maximum, minLength, maxLength, pattern, minItems, maxItems,
// format (date, date-time).

const DATE = /^\d{4}-\d{2}-\d{2}$/;
const DATE_TIME = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}(:\d{2}(\.\d+)?)?(Z|[+-]\d{2}:\d{2})$/;

function typeOf(v) {
  if (v === null) return 'null';
  if (Array.isArray(v)) return 'array';
  if (typeof v === 'number') return Number.isInteger(v) ? 'integer' : 'number';
  return typeof v;
}

function typeMatches(want, v) {
  const got = typeOf(v);
  if (want === got) return true;
  if (want === 'number' && got === 'integer') return true;
  return false;
}

function validDate(s) {
  if (!DATE.test(s)) return false;
  const d = new Date(`${s}T00:00:00Z`);
  return !Number.isNaN(d.getTime()) && d.toISOString().slice(0, 10) === s;
}

export function validate(schema, value, path = '$', errors = []) {
  if (schema === true || schema === undefined) return errors;
  if (schema.type !== undefined) {
    const types = Array.isArray(schema.type) ? schema.type : [schema.type];
    if (!types.some((t) => typeMatches(t, value))) {
      errors.push(`${path}: expected ${types.join('|')}, got ${typeOf(value)}`);
      return errors;
    }
  }
  if (schema.const !== undefined && JSON.stringify(schema.const) !== JSON.stringify(value)) {
    errors.push(`${path}: expected const ${JSON.stringify(schema.const)}`);
  }
  if (schema.enum && !schema.enum.some((e) => JSON.stringify(e) === JSON.stringify(value))) {
    errors.push(`${path}: ${JSON.stringify(value)} is not one of ${JSON.stringify(schema.enum)}`);
  }
  if (typeof value === 'number') {
    if (schema.minimum !== undefined && value < schema.minimum) errors.push(`${path}: below minimum ${schema.minimum}`);
    if (schema.maximum !== undefined && value > schema.maximum) errors.push(`${path}: above maximum ${schema.maximum}`);
  }
  if (typeof value === 'string') {
    if (schema.minLength !== undefined && value.length < schema.minLength) errors.push(`${path}: shorter than ${schema.minLength}`);
    if (schema.maxLength !== undefined && value.length > schema.maxLength) errors.push(`${path}: longer than ${schema.maxLength}`);
    if (schema.pattern !== undefined && !new RegExp(schema.pattern, 'u').test(value)) errors.push(`${path}: does not match ${schema.pattern}`);
    if (schema.format === 'date' && !validDate(value)) errors.push(`${path}: not a date (YYYY-MM-DD)`);
    if (schema.format === 'date-time' && (!DATE_TIME.test(value) || Number.isNaN(Date.parse(value)))) {
      errors.push(`${path}: not an ISO 8601 date-time with offset`);
    }
  }
  if (Array.isArray(value)) {
    if (schema.minItems !== undefined && value.length < schema.minItems) errors.push(`${path}: fewer than ${schema.minItems} items`);
    if (schema.maxItems !== undefined && value.length > schema.maxItems) errors.push(`${path}: more than ${schema.maxItems} items`);
    if (schema.items) value.forEach((item, i) => validate(schema.items, item, `${path}[${i}]`, errors));
  }
  if (typeOf(value) === 'object') {
    for (const key of schema.required ?? []) {
      if (!(key in value)) errors.push(`${path}: missing required property ${key}`);
    }
    const props = schema.properties ?? {};
    for (const [key, v] of Object.entries(value)) {
      if (props[key]) validate(props[key], v, `${path}.${key}`, errors);
      else if (schema.additionalProperties === false) errors.push(`${path}: unexpected property ${key}`);
    }
  }
  return errors;
}

export function getPath(obj, path) {
  if (path === '' || path === undefined) return obj;
  let cur = obj;
  for (const part of String(path).split('.')) {
    if (cur === null || cur === undefined) return undefined;
    cur = cur[/^\d+$/.test(part) && Array.isArray(cur) ? Number(part) : part];
  }
  return cur;
}

const norm = (s) => String(s).trim().replace(/\s+/g, ' ').toLowerCase();

function canon(v) {
  if (Array.isArray(v)) return v.map(canon);
  if (v && typeof v === 'object') {
    return Object.fromEntries(Object.keys(v).sort().map((k) => [k, canon(v[k])]));
  }
  return v;
}
const deepEq = (a, b) => JSON.stringify(canon(a)) === JSON.stringify(canon(b));

function itemKey(v) {
  return typeof v === 'string' ? `s:${norm(v)}` : `j:${JSON.stringify(canon(v))}`;
}

export function applyCheck(check, actualRoot) {
  const a = getPath(actualRoot, check.path);
  const want = check.value;
  switch (check.op) {
    case 'eq':
      return deepEq(a, want);
    case 'num':
      return typeof a === 'number' && Math.abs(a - want) <= (check.tol ?? 0.01);
    case 'str_ci':
      return typeof a === 'string' && norm(a) === norm(want);
    case 'set': {
      if (!Array.isArray(a) || a.length !== want.length) return false;
      const got = a.map(itemKey).sort();
      const exp = want.map(itemKey).sort();
      return got.every((k, i) => k === exp[i]);
    }
    case 'len':
      return Array.isArray(a) && a.length === want;
    default:
      throw new Error(`unknown check op ${check.op}`);
  }
}

export function scoreChecks(checks, actual) {
  const failed = [];
  for (const c of checks) {
    let ok = false;
    try { ok = applyCheck(c, actual); } catch { ok = false; }
    if (!ok) failed.push(c);
  }
  return { total: checks.length, passed: checks.length - failed.length, failed };
}

export function scoreRubric(checks, text) {
  const failed = [];
  const words = text.split(/\s+/).filter(Boolean).length;
  for (const c of checks) {
    let ok;
    switch (c.kind) {
      case 'heading':
        ok = text.split('\n').some((line) => new RegExp(c.pattern, c.flags ?? 'i').test(line));
        break;
      case 'regex':
        ok = new RegExp(c.pattern, c.flags ?? '').test(text);
        break;
      case 'absent':
        ok = !new RegExp(c.pattern, c.flags ?? '').test(text);
        break;
      case 'max_words':
        ok = words <= c.value;
        break;
      case 'min_words':
        ok = words >= c.value;
        break;
      default:
        throw new Error(`unknown rubric kind ${c.kind}`);
    }
    if (!ok) failed.push(c);
  }
  return { total: checks.length, passed: checks.length - failed.length, failed, words };
}
