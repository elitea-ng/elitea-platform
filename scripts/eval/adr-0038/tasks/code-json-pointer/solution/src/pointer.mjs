// RFC 6901 JSON Pointer helpers.

const INDEX_RE = /^(?:0|[1-9][0-9]*)$/;

function unescapeToken(raw) {
  if (/~(?![01])/.test(raw)) throw new Error(`invalid escape in token: ${raw}`);
  return raw.replace(/~1/g, '/').replace(/~0/g, '~');
}

function escapeToken(token) {
  return String(token).replace(/~/g, '~0').replace(/\//g, '~1');
}

export function parse(pointer) {
  if (typeof pointer !== 'string') throw new Error('pointer must be a string');
  if (pointer === '') return [];
  if (pointer[0] !== '/') throw new Error(`pointer must start with "/": ${pointer}`);
  return pointer.slice(1).split('/').map(unescapeToken);
}

export function compile(tokens) {
  return tokens.map((t) => '/' + escapeToken(t)).join('');
}

const isContainer = (v) => v !== null && typeof v === 'object';
const hasOwn = (obj, key) => Object.prototype.hasOwnProperty.call(obj, key);

function lookup(container, token) {
  if (Array.isArray(container)) {
    if (!INDEX_RE.test(token)) return undefined;
    const i = Number(token);
    return i < container.length ? container[i] : undefined;
  }
  if (isContainer(container)) return hasOwn(container, token) ? container[token] : undefined;
  return undefined;
}

function resolveParent(doc, tokens) {
  let cur = doc;
  for (const token of tokens.slice(0, -1)) {
    cur = lookup(cur, token);
    if (!isContainer(cur)) throw new Error(`path not found at "${token}"`);
  }
  return cur;
}

export function get(doc, pointer) {
  const tokens = parse(pointer);
  let cur = doc;
  for (const token of tokens) {
    cur = lookup(cur, token);
    if (cur === undefined) return undefined;
  }
  return cur;
}

export function set(doc, pointer, value) {
  const tokens = parse(pointer);
  if (tokens.length === 0) return value;
  const parent = resolveParent(doc, tokens);
  const last = tokens[tokens.length - 1];
  if (Array.isArray(parent)) {
    if (last === '-') {
      parent.push(value);
    } else {
      if (!INDEX_RE.test(last)) throw new Error(`invalid array index: ${last}`);
      const i = Number(last);
      if (i > parent.length) throw new Error(`array index out of range: ${last}`);
      parent[i] = value;
    }
  } else {
    parent[last] = value;
  }
  return doc;
}

export function remove(doc, pointer) {
  const tokens = parse(pointer);
  if (tokens.length === 0) throw new Error('cannot remove the document root');
  const parent = resolveParent(doc, tokens);
  const last = tokens[tokens.length - 1];
  if (Array.isArray(parent)) {
    if (!INDEX_RE.test(last)) throw new Error(`invalid array index: ${last}`);
    const i = Number(last);
    if (i >= parent.length) throw new Error(`array index out of range: ${last}`);
    return parent.splice(i, 1)[0];
  }
  if (!hasOwn(parent, last)) throw new Error(`path not found: ${pointer}`);
  const removed = parent[last];
  delete parent[last];
  return removed;
}
