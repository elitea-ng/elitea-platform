#!/usr/bin/env node
// No deploy file gives a signing secret a literal value (#2958).
//
// The compose files once shipped APPLICATION_SECRET_KEY, the identity secrets
// and a vault master key as fixed values. A deployment copied from them had
// signing keys anyone can read in this public repository. Now every one of
// them reads its value from the environment, and the stack scripts generate
// it (deploy/scripts/lib/stack-secrets.sh). This check keeps it that way.
//
// It reads every tracked deploy/**/*.yml and *.yaml and refuses, for each
// variable below, an assignment whose value is a literal:
//   VAR: "value"          refused
//   VAR: "${VAR}"         allowed (the environment supplies it)
//   VAR: "${VAR:-}"       allowed
//   VAR: "${VAR:?why}"    allowed
//   VAR: "${VAR:-value}"  refused (a literal default is a literal)
//   VAR:                  allowed (a mapping: a Secret reference follows)
//   VAR: ""               allowed (empty: the service refuses to start)
// The same rule applies to the list form `- VAR=value`. The unwrapped
// vault-key opt-out is refused as a literal "true" as well: next to a real
// sign-in configuration the services refuse it, and a committed "true" is
// how it would reach one.
//
// Helm templates are not read (their values come from values files, and the
// chart refuses weak values itself: templates/_secret-strength.tpl).
//
// Run with the runner's node; only the standard library.

import path from "node:path";
import { fileURLToPath } from "node:url";

import { isEntrypoint, readStrictUtf8, splitlines, trackedFiles } from "./py-compat.mjs";

const REPOSITORY_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..", "..");

// The signing secrets and the vault master key, and every sub-application
// host's <PREFIX>IDENTITY_SECRET.
const SECRET = /^(APPLICATION_SECRET_KEY|GATEWAY_IDENTITY_SECRET|GATEWAY_HOP_SECRET|SECRETS_MASTER_KEY|(ELITEA_[A-Z0-9]+_)?IDENTITY_SECRET)$/;
const OPT_OUT = "ELITEA_DEV_ALLOW_UNWRAPPED_SECRETS";

const ASSIGNMENT = /^\s*(?:-\s+)?["']?([A-Z][A-Z0-9_]*)["']?\s*(?::\s|:$|=)(.*)$/;

export function isDeployFile(name) {
  return name.startsWith("deploy/") && /\.ya?ml$/.test(name) && !name.includes("/templates/");
}

function unquote(value) {
  let v = value.replace(/\s+#.*$/, "").trim();
  if ((v.startsWith('"') && v.endsWith('"')) || (v.startsWith("'") && v.endsWith("'"))) v = v.slice(1, -1);
  return v;
}

// The literal part of a value, or "" when the environment supplies all of it.
export function literalOf(raw) {
  const value = unquote(raw);
  if (value === "") return "";
  const interpolation = /^\$\{([A-Za-z_][A-Za-z0-9_]*)(?:(:?[-?])(.*))?\}$/.exec(value);
  if (!interpolation) return value;
  const [, , operator, rest] = interpolation;
  if (operator === undefined || operator.endsWith("?")) return "";
  return rest ?? "";
}

export function scan(root, files) {
  const violations = [];
  for (const name of files) {
    if (!isDeployFile(name)) continue;
    const text = readStrictUtf8(path.join(root, name));
    if (text === null) continue;
    splitlines(text).forEach((line, index) => {
      if (/^\s*#/.test(line)) return;
      const match = ASSIGNMENT.exec(line);
      if (!match) return;
      const [, variable, raw] = match;
      const literal = literalOf(raw);
      if (SECRET.test(variable) && literal !== "") {
        violations.push(`${name}:${index + 1}: ${variable} has a literal value; read it from the environment ("\${${variable}:?...}") or a Secret`);
      }
      if (variable === OPT_OUT && literal.toLowerCase() === "true") {
        violations.push(`${name}:${index + 1}: ${OPT_OUT} is set to a literal "true"; generate SECRETS_MASTER_KEY instead`);
      }
    });
  }
  return violations;
}

export function main(root = REPOSITORY_ROOT, log = console.log, files = trackedFiles(root)) {
  const deployFiles = files.filter(isDeployFile);
  if (deployFiles.length === 0) {
    log("::error::no tracked deploy/**/*.yml files to read; a check that reads nothing proves nothing");
    return 1;
  }
  const violations = scan(root, deployFiles);
  for (const violation of violations) log(`::error::${violation}`);
  if (violations.length) return 1;
  log(`deploy-signing-secrets: ${deployFiles.length} deploy files carry no literal signing secret`);
  return 0;
}

if (isEntrypoint(import.meta.url)) {
  process.exitCode = main();
}
