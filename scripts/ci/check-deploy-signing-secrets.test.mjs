// Scenarios for check-deploy-signing-secrets.mjs. The workflow step runs the
// check against the repository itself, so no scenario here repeats that scan.

import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { test } from "node:test";

import { literalOf, main } from "./check-deploy-signing-secrets.mjs";

function tree(t, files) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "deploy-secrets-"));
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));
  for (const [name, text] of Object.entries(files)) {
    fs.mkdirSync(path.dirname(path.join(root, name)), { recursive: true });
    fs.writeFileSync(path.join(root, name), text);
  }
  return [root, Object.keys(files)];
}

function run(root, files) {
  const lines = [];
  return [main(root, (line) => lines.push(line), files), lines.join("\n")];
}

test("a literal signing secret in a compose file is a violation with its line", (t) => {
  const [root, files] = tree(t, {
    "deploy/docker-compose.yml": [
      "services:",
      "  elitea-main:",
      "    environment:",
      '      APPLICATION_SECRET_KEY: "changeme"',
      "      ELITEA_DEEPWIKI_IDENTITY_SECRET: 'e2e-deepwiki-identity'",
      "",
    ].join("\n"),
  });
  const [code, out] = run(root, files);
  assert.equal(code, 1);
  assert.match(out, /docker-compose\.yml:4: APPLICATION_SECRET_KEY has a literal value/);
  assert.match(out, /docker-compose\.yml:5: ELITEA_DEEPWIKI_IDENTITY_SECRET has a literal value/);
});

test("a literal default inside an interpolation is a literal", (t) => {
  const [root, files] = tree(t, {
    "deploy/docker-compose.standalone-full.yml": '      GATEWAY_IDENTITY_SECRET: "${GATEWAY_IDENTITY_SECRET:-standalone-dev-identity-secret}"\n',
  });
  const [code, out] = run(root, files);
  assert.equal(code, 1);
  assert.match(out, /GATEWAY_IDENTITY_SECRET has a literal value/);
});

test("the list form, a sub-application prefix and a values file are read too", (t) => {
  const [root, files] = tree(t, {
    "deploy/docker-compose.extra.yml": "    environment:\n      - ELITEA_INVENTORY_IDENTITY_SECRET=kind-inventory-identity\n",
    "deploy/kind/values-kind.yaml": "main:\n  env:\n    SECRETS_MASTER_KEY: AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\n",
  });
  const [code, out] = run(root, files);
  assert.equal(code, 1);
  assert.match(out, /docker-compose\.extra\.yml:2: ELITEA_INVENTORY_IDENTITY_SECRET/);
  assert.match(out, /values-kind\.yaml:3: SECRETS_MASTER_KEY/);
});

test("a literal unwrapped opt-out is refused", (t) => {
  const [root, files] = tree(t, {
    "deploy/docker-compose.e2e-standalone.yml": '      ELITEA_DEV_ALLOW_UNWRAPPED_SECRETS: "true"\n',
  });
  const [code, out] = run(root, files);
  assert.equal(code, 1);
  assert.match(out, /ELITEA_DEV_ALLOW_UNWRAPPED_SECRETS is set to a literal "true"/);
});

test("values from the environment, empty values, Secret references and comments pass", (t) => {
  const [root, files] = tree(t, {
    "deploy/docker-compose.yml": [
      '      APPLICATION_SECRET_KEY: "${APPLICATION_SECRET_KEY:?set a random 32-byte value}"',
      '      GATEWAY_IDENTITY_SECRET: "${GATEWAY_IDENTITY_SECRET:-}"',
      "      SECRETS_MASTER_KEY: ${SECRETS_MASTER_KEY}",
      '      ELITEA_DEEPWIKI_IDENTITY_SECRET: ""',
      '      # APPLICATION_SECRET_KEY: "changeme" was the old value',
      "",
    ].join("\n"),
    "deploy/helm/elitea/values.yaml": "main:\n  secrets:\n    ELITEA_DEEPWIKI_IDENTITY_SECRET:\n      secretName: elitea-deepwiki-secrets\n",
    "deploy/helm/elitea/templates/x.yaml": "        - name: APPLICATION_SECRET_KEY\n          value: {{ .Values.x }}\n",
  });
  const [code, out] = run(root, files);
  assert.equal(code, 0, out);
});

test("a check that reads no deploy file fails", (t) => {
  const [root, files] = tree(t, { "README.md": "nothing\n" });
  const [code, out] = run(root, files);
  assert.equal(code, 1);
  assert.match(out, /reads nothing/);
});

test("literalOf separates the environment from a literal", () => {
  assert.equal(literalOf('"${A}"'), "");
  assert.equal(literalOf('"${A:?why}"'), "");
  assert.equal(literalOf('"${A-}"'), "");
  assert.equal(literalOf('"${A-x}"'), "x");
  assert.equal(literalOf("plain # trailing comment"), "plain");
});
