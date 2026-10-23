// What the scripts/ci checks need from the Python they replaced, in one place.
//
// The checks were Python with PyYAML until 2026-10-20 (owner decision: CI
// scripts run with the runner's own `node`, no Python, no Go). Their messages
// are grepped by tests and read in logs, and their YAML reads decided what
// they checked, so two things had to come across exactly:
//
//   loadYaml / loadYamlNodes  YAML read the way PyYAML's safe_load reads it.
//                             PyYAML is YAML 1.1: an unquoted `on:` key is the
//                             boolean True, `yes`/`no`/`on`/`off` are
//                             booleans, `<<` merges, a repeated key keeps the
//                             last value. The `yaml` package's default is YAML
//                             1.2, where `on` is a string. Its 1.1 schema is
//                             closer, but its bool and number resolvers are
//                             wider than PyYAML's (`y`/`n` booleans, `1e5`
//                             and a bare `.` floats, `09` an int); they are
//                             replaced by PyYAML's own below.
//                             Mapping keys come out as strings, as in any JS
//                             object: PyYAML's True key is "true" here. A tag
//                             the schema does not know (compose's `!reset`,
//                             `!override`) reads as untagged, a scalar as its
//                             string, as the Python loaders read them.
//   pyRepr / pyStr            Python's repr() and str() of what a check prints,
//                             so a message that showed `'value'` or `None`
//                             still does. They live in ./py-compat.mjs, which
//                             needs no npm install, and are re-exported here.

import YAML from "yaml";

const BOOL = "tag:yaml.org,2002:bool";
const FLOAT = "tag:yaml.org,2002:float";
const INT = "tag:yaml.org,2002:int";

// PyYAML's implicit resolvers (yaml/resolver.py) and constructors
// (yaml/constructor.py SafeConstructor) for booleans, ints and floats, as
// scalar tags of our own. The yaml package's 1.1 resolvers for these are wider
// than PyYAML's (`y`/`n` booleans, `1e5` and a bare `.` floats, `09` an int),
// so they are replaced outright rather than patched: nothing here depends on
// how the package spells its own regexes.
//
// A float needs a digit before the dot unless the dot leads one (`.5`; a bare
// `.` or `-.5` is a string) and a signed exponent after a dot (`1e5` and
// `1.0e5` are strings). A decimal int has no leading zero (`09` is a string;
// `010` is octal 8) and a sexagesimal one starts 1-9 (`08:30` is a string).
const PYYAML_TRUE = /^(?:yes|Yes|YES|true|True|TRUE|on|On|ON)$/;
const PYYAML_FALSE = /^(?:no|No|NO|false|False|FALSE|off|Off|OFF)$/;
const PYYAML_INT =
  /^(?:[-+]?0b[0-1_]+|[-+]?0[0-7_]+|[-+]?(?:0|[1-9][0-9_]*)|[-+]?0x[0-9a-fA-F_]+|[-+]?[1-9][0-9_]*(?::[0-5]?[0-9])+)$/;
const PYYAML_FLOAT =
  /^(?:[-+]?(?:[0-9][0-9_]*)\.[0-9_]*(?:[eE][-+][0-9]+)?|\.[0-9][0-9_]*(?:[eE][-+][0-9]+)?|[-+]?[0-9][0-9_]*(?::[0-5]?[0-9])+\.[0-9_]*|[-+]?\.(?:inf|Inf|INF)|\.(?:nan|NaN|NAN))$/;

// construct_yaml_int / construct_yaml_float.
function sexagesimal(digits, parse) {
  return digits.split(":").reduce((total, part) => total * 60 + parse(part), 0);
}

function constructInt(source) {
  let value = source.replace(/_/g, "");
  let sign = 1;
  if (value[0] === "-") sign = -1;
  if (value[0] === "-" || value[0] === "+") value = value.slice(1);
  if (value === "0") return 0;
  if (value.startsWith("0b")) return sign * Number.parseInt(value.slice(2), 2);
  if (value.startsWith("0x")) return sign * Number.parseInt(value.slice(2), 16);
  if (value[0] === "0") return sign * Number.parseInt(value, 8);
  if (value.includes(":")) return sign * sexagesimal(value, (p) => Number.parseInt(p, 10));
  return sign * Number.parseInt(value, 10);
}

function constructFloat(source) {
  let value = source.replace(/_/g, "").toLowerCase();
  let sign = 1;
  if (value[0] === "-") sign = -1;
  if (value[0] === "-" || value[0] === "+") value = value.slice(1);
  if (value === ".inf") return sign * Number.POSITIVE_INFINITY;
  if (value === ".nan") return Number.NaN;
  if (value.includes(":")) return sign * sexagesimal(value, (p) => Number.parseFloat(p));
  return sign * Number.parseFloat(value);
}

const PYYAML_SCALARS = [
  { tag: BOOL, test: PYYAML_TRUE, resolve: () => true, identify: (v) => v === true },
  { tag: BOOL, test: PYYAML_FALSE, resolve: () => false, identify: (v) => v === false },
  { tag: INT, test: PYYAML_INT, resolve: constructInt, identify: (v) => Number.isInteger(v) },
  { tag: FLOAT, test: PYYAML_FLOAT, resolve: constructFloat, identify: (v) => typeof v === "number" },
].map((tag) => ({ default: true, ...tag }));

// The yaml package's 1.1 schema's bool, int and float tags: what the
// replacement above stands in for. A package release that changes this set
// would leave a resolver of its own in front of PyYAML's, so a different set
// fails the read instead of being skipped.
const REPLACED = { [BOOL]: ["", ""], [INT]: ["", "BIN", "HEX", "OCT", "TIME"], [FLOAT]: ["", "", "EXP", "TIME"] };

export function pyyamlTags(extraTags) {
  return (tags) => {
    const seen = { [BOOL]: [], [INT]: [], [FLOAT]: [] };
    const out = [];
    let at = -1;
    for (const tag of tags) {
      if (Object.hasOwn(seen, tag.tag)) {
        seen[tag.tag].push(tag.format || "");
        if (at < 0) at = out.length;
      } else {
        out.push(tag);
      }
    }
    for (const [name, formats] of Object.entries(REPLACED)) {
      const got = seen[name].sort();
      if (JSON.stringify(got) !== JSON.stringify(formats)) {
        throw new Error(
          `ci-compat: the yaml package's 1.1 schema has ${name} tags with formats ${JSON.stringify(got)}, expected ${JSON.stringify(formats)}; review the PyYAML resolvers in ci-compat.mjs against it`,
        );
      }
    }
    out.splice(at, 0, ...PYYAML_SCALARS);
    return [...out, ...(extraTags || [])];
  };
}

function options(extraTags) {
  return {
    version: "1.1",
    customTags: pyyamlTags(extraTags),
    uniqueKeys: false,
    merge: true,
    logLevel: "error",
    prettyErrors: false,
  };
}

/** yaml.safe_load(text): one document, null when empty. Throws on a parse error. */
export function loadYaml(text, { extraTags } = {}) {
  const doc = YAML.parseDocument(text, options(extraTags));
  if (doc.errors.length) throw doc.errors[0];
  const value = doc.toJS({ maxAliasCount: -1 });
  return value === undefined ? null : value;
}

/** yaml.compose_all(text): the node trees of every document. Throws on a parse error. */
export function loadYamlNodes(text) {
  const docs = YAML.parseAllDocuments(text, options());
  const list = Array.isArray(docs) ? docs : [docs];
  for (const doc of list) {
    if (doc.errors && doc.errors.length) throw doc.errors[0];
  }
  return list;
}

export { YAML };
export { isEntrypoint, pyRepr, pyStr, splitlines } from "./py-compat.mjs";
