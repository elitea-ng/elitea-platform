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
//                             narrowed to PyYAML's below.
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

// PyYAML's resolver (yaml/resolver.py), not the YAML 1.1 spec's longer list.
const PYYAML_TRUE = /^(?:yes|Yes|YES|true|True|TRUE|on|On|ON)$/;
const PYYAML_FALSE = /^(?:no|No|NO|false|False|FALSE|off|Off|OFF)$/;
// PyYAML's numbers (yaml/resolver.py), where the yaml package's 1.1
// resolvers are wider. A float needs a digit before the dot unless the dot
// leads one (`.5`; a bare `.` or `-.5` is a string) and a signed exponent after
// a dot (`1e5` and `1.0e5` are strings). A decimal int has no leading zero
// (`09` is a string; `010` is octal 8) and a sexagesimal one starts 1-9
// (`08:30` is a string). Keyed by the yaml package's own resolver source.
const YAML11_NUMBERS = {
  "^[-+]?(?:[0-9][0-9_]*)?(?:\\.[0-9_]*)?[eE][-+]?[0-9]+$":
    /^(?:[-+]?[0-9][0-9_]*\.[0-9_]*[eE][-+][0-9]+|\.[0-9][0-9_]*[eE][-+][0-9]+)$/,
  "^[-+]?(?:[0-9][0-9_]*)?\\.[0-9_]*$": /^(?:[-+]?[0-9][0-9_]*\.[0-9_]*|\.[0-9][0-9_]*)$/,
  "^[-+]?[0-9][0-9_]*$": /^[-+]?(?:0|[1-9][0-9_]*)$/,
  "^[-+]?[0-9][0-9_]*(?::[0-5]?[0-9])+$": /^[-+]?[1-9][0-9_]*(?::[0-5]?[0-9])+$/,
};

function pyyamlTags(extraTags) {
  return (tags) => {
    const out = [];
    for (const tag of tags) {
      if (tag.tag === BOOL && tag.test) {
        const truthy = tag.test.test("true");
        out.push({ ...tag, test: truthy ? PYYAML_TRUE : PYYAML_FALSE });
      } else if ((tag.tag === FLOAT || tag.tag === INT) && tag.test && YAML11_NUMBERS[tag.test.source]) {
        out.push({ ...tag, test: YAML11_NUMBERS[tag.test.source] });
      } else {
        out.push(tag);
      }
    }
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
export { pyRepr, pyStr, splitlines } from "./py-compat.mjs";
