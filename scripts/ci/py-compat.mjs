// Python's repr(), str() and str.splitlines() for the scripts/ci checks, so a message
// that showed `'value'` or `None` before the port from Python still does.
// Only the language: a script that imports this needs no `npm ci`
// (./ci-compat.mjs, which reads YAML, re-exports it).

function reprString(text) {
  const quote = text.includes("'") && !text.includes('"') ? '"' : "'";
  let out = quote;
  for (const ch of text) {
    const code = ch.codePointAt(0);
    if (ch === "\\") out += "\\\\";
    else if (ch === quote) out += "\\" + ch;
    else if (ch === "\n") out += "\\n";
    else if (ch === "\r") out += "\\r";
    else if (ch === "\t") out += "\\t";
    else if (code < 0x20 || code === 0x7f) out += "\\x" + code.toString(16).padStart(2, "0");
    else if (code >= 0x80 && code < 0xa0) out += "\\x" + code.toString(16).padStart(2, "0");
    else out += ch;
  }
  return out + quote;
}

function reprNumber(value) {
  if (Number.isInteger(value) && !Object.is(value, -0)) return String(value);
  if (Number.isNaN(value)) return "nan";
  if (value === Infinity) return "inf";
  if (value === -Infinity) return "-inf";
  const text = String(value);
  return /[.e]/.test(text) ? text : `${text}.0`;
}

/** Python's repr() of a JSON-shaped value. */
export function pyRepr(value) {
  if (value === null || value === undefined) return "None";
  if (value === true) return "True";
  if (value === false) return "False";
  if (typeof value === "number") return reprNumber(value);
  if (typeof value === "string") return reprString(value);
  if (Array.isArray(value)) return `[${value.map(pyRepr).join(", ")}]`;
  if (value instanceof Date) return `datetime.datetime(${value.toISOString()})`;
  if (typeof value === "object") {
    return `{${Object.entries(value).map(([k, v]) => `${pyRepr(k)}: ${pyRepr(v)}`).join(", ")}}`;
  }
  return String(value);
}

/** Python's str(): a string as itself, anything else as its repr. */
export function pyStr(value) {
  return typeof value === "string" ? value : pyRepr(value);
}

// str.splitlines(): every line boundary Python knows, \r\n as one.
const LINE_BREAK = /\r\n|[\n\r\v\f\x1c\x1d\x1e\x85\u2028\u2029]/;

/** Python's str.splitlines(): no trailing empty line, \r\n counts once. */
export function splitlines(text) {
  if (text === "") return [];
  const lines = text.split(LINE_BREAK);
  if (LINE_BREAK.test(text.slice(-1)) || text.endsWith("\r\n")) lines.pop();
  return lines;
}

/** fnmatch.fnmatchcase(name, pattern): `*` and `?` cross `/`, `[!x]` negates. */
export function fnmatchcase(name, pattern) {
  let out = "";
  let i = 0;
  const n = pattern.length;
  while (i < n) {
    const c = pattern[i];
    i += 1;
    if (c === "*") {
      if (!out.endsWith("[\\s\\S]*")) out += "[\\s\\S]*";
    } else if (c === "?") {
      out += "[\\s\\S]";
    } else if (c === "[") {
      let j = i;
      if (j < n && pattern[j] === "!") j += 1;
      if (j < n && pattern[j] === "]") j += 1;
      while (j < n && pattern[j] !== "]") j += 1;
      if (j >= n) {
        out += "\\[";
      } else {
        let stuff = pattern.slice(i, j).replaceAll("\\", "\\\\").replaceAll("]", "\\]");
        i = j + 1;
        if (stuff[0] === "!") stuff = "^" + stuff.slice(1);
        else if (stuff[0] === "^" || stuff[0] === "[") stuff = "\\" + stuff;
        out += `[${stuff}]`;
      }
    } else {
      out += c.replace(/[.*+?^${}()|[\]\\/-]/g, "\\$&");
    }
  }
  return new RegExp(`^(?:${out})$`).test(name);
}
