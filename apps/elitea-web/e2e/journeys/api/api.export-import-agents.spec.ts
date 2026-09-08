/**
 * EXP: an agent survives being exported to a `.agent.md` file and imported
 * back through the platform's own API.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT THIS PORTS
 * ─────────────────────────────────────────────────────────────────────────────
 * `EliteaAI/elitea-testing-public`,
 * `automation/tests/api/export_import/test_export_import_prompts.py` — all nine
 * tests, one journey each, keeping the legacy EXP-00n identifiers so a failure
 * here can be traced to the case it came from:
 *
 *   EXP-001 test_export_import_roundtrip_basic
 *   EXP-002 test_export_import_preserves_all_fields
 *   EXP-003 test_export_format_is_valid_yaml_frontmatter
 *   EXP-004 test_import_with_conflicting_name_creates_new
 *   EXP-005 test_import_preserves_original_when_conflict
 *   EXP-006 test_export_import_empty_optional_fields
 *   EXP-007 test_export_import_special_characters_in_name
 *   EXP-008 test_export_import_large_instructions
 *   EXP-009 test_import_via_constructed_payload
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHY THESE ARE HERE AND NOT IN A UNIT TEST
 * ─────────────────────────────────────────────────────────────────────────────
 * The trip crosses three subsystems that are each correct about their own half:
 * the exporter renders one file out of five tables, the client parses the file,
 * the import re-derives rows from the parse. Every one of them can lose a field
 * while answering 2xx, and the import in particular answers 201 with a
 * per-entity error list inside the body. Two Go defects came out of the first
 * run of exactly these nine cases against this stack: the version summary had
 * dropped `instructions` and `meta` (issue 844), and the import refused the
 * `agent_type` the export itself writes (issue 845). Neither is visible from
 * either side alone.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT THIS RUN LEAVES BEHIND
 * ─────────────────────────────────────────────────────────────────────────────
 * Nothing. Every journey creates its own `autotest_*` agents and deletes them
 * in a `finally`, including the ones an import created. No journey reads or
 * writes another's rows, and none depends on the order they run in.
 */
import { test, expect } from '@playwright/test';

import { AUTOTEST_PREFIX } from '../../fixtures/api';
import {
  FIXTURE_MODEL,
  applicationIsGone,
  buildImportPayload,
  buildImportPayloadFromExport,
  createApplication,
  defaultVersionOf,
  deleteApplication,
  exportMarkdown,
  importWizard,
  importedAgentId,
  normaliseWhitespace,
  readApplication,
  variablesAsRecord,
} from '../../fixtures/exportImport';
import { STORAGE_STATE } from '../../../playwright.config';

test.use({ storageState: STORAGE_STATE.admin });

/** A name nobody else in the run can collide with. */
function autotestName(tag: string): string {
  return `${AUTOTEST_PREFIX}${tag}_${Math.random().toString(36).slice(2, 8)}`;
}

/**
 * The legacy suite slept 0.5 s after a delete "to settle". A sleep is either
 * too short (flake) or too long (every case pays it), and the harness forbids
 * `waitForTimeout` for that reason. The delete has a server-side read that
 * answers the question directly, so this polls it.
 */
async function expectDeleted(
  request: Parameters<typeof applicationIsGone>[0],
  id: string,
): Promise<void> {
  await expect
    .poll(async () => applicationIsGone(request, id), {
      timeout: 15_000,
      message: `application ${id} is still readable after its DELETE`,
    })
    .toBe(true);
}

test('EXP-001: an exported agent imports back with its core fields intact', async ({ request }) => {
  const name = autotestName('export_basic');
  let createdId: string | undefined;
  let importedId: string | undefined;
  try {
    createdId = await createApplication(request, {
      name,
      description: 'Roundtrip test',
      instructions: 'Answer questions about Python testing.',
      temperature: 0.5,
      maxTokens: 2048,
      variables: [
        { name: 'language', value: 'Python' },
        { name: 'framework', value: 'pytest' },
      ],
    });

    const exported = await exportMarkdown(request, createdId);
    expect(exported.text.length, 'the export answered an empty document').toBeGreaterThan(0);
    expect(exported.parsed.frontmatter['name']).toBe(name);
    // The agent's instructions ARE the markdown body. This is the assertion
    // that says the format still separates the two.
    expect(exported.parsed.body).toBe('Answer questions about Python testing.');

    await deleteApplication(request, createdId);
    await expectDeleted(request, createdId);
    const deletedId = createdId;
    createdId = undefined;

    importedId = importedAgentId(await importWizard(request, buildImportPayloadFromExport(exported.parsed)));
    expect(importedId, 'the import reused the deleted id').not.toBe(deletedId);

    const imported = await readApplication(request, importedId);
    expect(imported.name).toBe(name);
    expect(imported.description).toBe('Roundtrip test');

    const version = defaultVersionOf(imported);
    expect(version.instructions).toBe('Answer questions about Python testing.');
    expect(version.llm_settings?.['model_name']).toBe(FIXTURE_MODEL);
    expect(version.llm_settings?.['temperature']).toBe(0.5);
    expect(version.llm_settings?.['max_tokens']).toBe(2048);
    expect(variablesAsRecord(version)).toStrictEqual({ language: 'Python', framework: 'pytest' });
  } finally {
    if (createdId !== undefined) await deleteApplication(request, createdId);
    if (importedId !== undefined) await deleteApplication(request, importedId);
  }
});

test('EXP-002: every populated field survives the round trip', async ({ request }) => {
  const name = autotestName('full_fields');
  const instructions = [
    'You are a comprehensive test agent.',
    '',
    '## Guidelines',
    '- Always be helpful',
    '- Use markdown in responses',
    '- Include code examples when relevant',
  ].join('\n');
  let createdId: string | undefined;
  let importedId: string | undefined;
  try {
    createdId = await createApplication(request, {
      name,
      description: 'Full field coverage agent',
      instructions,
      temperature: 0.3,
      maxTokens: 4096,
      variables: [
        { name: 'topic', value: 'automation' },
        { name: 'style', value: 'technical' },
        { name: 'lang', value: 'en' },
      ],
      welcomeMessage: 'Welcome! How can I help with testing?',
      conversationStarters: ['Run tests', 'Debug issue'],
    });

    // The baseline for the comparison is what the SERVER stored, not what the
    // create request asked for: a field the create itself dropped would
    // otherwise be reported as a round-trip loss, in the wrong subsystem.
    const original = await readApplication(request, createdId);
    const originalVersion = defaultVersionOf(original);

    const exported = await exportMarkdown(request, createdId);
    await deleteApplication(request, createdId);
    await expectDeleted(request, createdId);
    createdId = undefined;

    importedId = importedAgentId(await importWizard(request, buildImportPayloadFromExport(exported.parsed)));
    const imported = await readApplication(request, importedId);
    const importedVersion = defaultVersionOf(imported);

    expect(imported.name).toBe(original.name);
    expect(imported.description).toBe(original.description);
    expect(importedVersion.instructions).toBe(originalVersion.instructions);
    expect(importedVersion.agent_type).toBe(originalVersion.agent_type);
    expect(importedVersion.welcome_message).toBe(originalVersion.welcome_message);
    expect(importedVersion.conversation_starters).toStrictEqual(originalVersion.conversation_starters);
    expect(variablesAsRecord(importedVersion)).toStrictEqual(variablesAsRecord(originalVersion));
    for (const setting of ['model_name', 'temperature', 'max_tokens'] as const) {
      expect(
        importedVersion.llm_settings?.[setting],
        `llm_settings.${setting} did not survive the round trip`,
      ).toStrictEqual(originalVersion.llm_settings?.[setting]);
    }
  } finally {
    if (createdId !== undefined) await deleteApplication(request, createdId);
    if (importedId !== undefined) await deleteApplication(request, importedId);
  }
});

test('EXP-003: the export is YAML frontmatter followed by the instructions', async ({ request }) => {
  const name = autotestName('fmt');
  let createdId: string | undefined;
  try {
    createdId = await createApplication(request, {
      name,
      instructions: 'Test instructions for format validation.',
    });

    const exported = await exportMarkdown(request, createdId);
    expect(exported.text.startsWith('---\n'), 'the export must open with the YAML delimiter').toBe(true);

    // The three keys any importer — this platform's, pylon's, or a hand-rolled
    // one — has to find to reconstruct the agent.
    expect(Object.keys(exported.parsed.frontmatter)).toEqual(
      expect.arrayContaining(['name', 'model', 'agent_type']),
    );
    expect(exported.parsed.body).toBe('Test instructions for format validation.');
  } finally {
    if (createdId !== undefined) await deleteApplication(request, createdId);
  }
});

test('EXP-004: importing over a name that already exists creates a second agent', async ({
  request,
}) => {
  const name = autotestName('conflict');
  let originalId: string | undefined;
  let importedId: string | undefined;
  try {
    originalId = await createApplication(request, {
      name,
      description: 'Original agent',
      instructions: 'Original instructions.',
    });

    const exported = await exportMarkdown(request, originalId);
    // Deliberately NOT deleted: the point is what happens when the name is
    // taken. The legacy case branched here and xfailed if the platform
    // refused; this one asserts, because the behaviour is settled — nothing
    // makes `applications.name` unique, and the import wizard's own product
    // promise is that a re-import never overwrites.
    importedId = importedAgentId(await importWizard(request, buildImportPayloadFromExport(exported.parsed)));

    expect(importedId, 'the import overwrote the existing agent instead of adding one').not.toBe(
      originalId,
    );
    expect((await readApplication(request, importedId)).name).toBe(name);
  } finally {
    if (originalId !== undefined) await deleteApplication(request, originalId);
    if (importedId !== undefined) await deleteApplication(request, importedId);
  }
});

test('EXP-005: the existing agent is untouched by an import that collides with it', async ({
  request,
}) => {
  const name = autotestName('preserve');
  let originalId: string | undefined;
  let importedId: string | undefined;
  try {
    originalId = await createApplication(request, {
      name,
      description: 'Original description',
      instructions: 'Original instructions.',
      temperature: 0.3,
    });
    const before = await readApplication(request, originalId);

    const exported = await exportMarkdown(request, originalId);
    importedId = importedAgentId(await importWizard(request, buildImportPayloadFromExport(exported.parsed)));

    const after = await readApplication(request, originalId);
    expect(after.id).toBe(originalId);
    expect(after.name).toBe(before.name);
    expect(after.description).toBe(before.description);
    expect(defaultVersionOf(after).instructions).toBe(defaultVersionOf(before).instructions);
    expect(importedId).not.toBe(originalId);
  } finally {
    if (originalId !== undefined) await deleteApplication(request, originalId);
    if (importedId !== undefined) await deleteApplication(request, importedId);
  }
});

test('EXP-006: an agent whose optional fields are empty round-trips as empty', async ({ request }) => {
  const name = autotestName('empty');
  let createdId: string | undefined;
  let importedId: string | undefined;
  try {
    createdId = await createApplication(request, {
      name,
      description: '',
      instructions: 'Minimal agent.',
      variables: [],
      welcomeMessage: '',
      conversationStarters: [],
    });

    const exported = await exportMarkdown(request, createdId);
    await deleteApplication(request, createdId);
    await expectDeleted(request, createdId);
    createdId = undefined;

    importedId = importedAgentId(await importWizard(request, buildImportPayloadFromExport(exported.parsed)));
    const imported = await readApplication(request, importedId);
    const version = defaultVersionOf(imported);

    expect(imported.name).toBe(name);
    expect(version.instructions).toBe('Minimal agent.');
    // Empty, not defaulted. An import that substituted a placeholder for an
    // absent value would answer 201 and change the agent.
    expect(imported.description ?? '').toBe('');
    expect(version.variables ?? []).toStrictEqual([]);
    expect(version.welcome_message ?? '').toBe('');
  } finally {
    if (createdId !== undefined) await deleteApplication(request, createdId);
    if (importedId !== undefined) await deleteApplication(request, importedId);
  }
});

test('EXP-007: special characters and unicode survive the round trip', async ({ request }) => {
  // The legacy case's ASCII punctuation — the characters a YAML writer or an
  // HTML escaper is most likely to mangle — plus non-Latin text and an emoji,
  // because the document is UTF-8 and nothing else in the suite says so.
  const name = `${AUTOTEST_PREFIX}sp&ci@l-${Math.random().toString(36).slice(2, 6)}`;
  const description = `Agent with special chars: <>&"' — ключи, 日本語, 😀`;
  const instructions = `Handle edge cases with special chars: <>&"'\nAnd unicode: ключи 日本語 😀`;
  let createdId: string | undefined;
  let importedId: string | undefined;
  try {
    createdId = await createApplication(request, { name, description, instructions });

    const exported = await exportMarkdown(request, createdId);
    await deleteApplication(request, createdId);
    await expectDeleted(request, createdId);
    createdId = undefined;

    importedId = importedAgentId(await importWizard(request, buildImportPayloadFromExport(exported.parsed)));
    const imported = await readApplication(request, importedId);

    expect(imported.name).toBe(name);
    expect(imported.description).toBe(description);
    // Read off `versions[]`, not off `version_details`. That array used to
    // carry no `instructions` at all (issue 844) and this is the assertion
    // that found it — the detail block one key away was already correct.
    expect(imported.versions?.[0]?.instructions).toBe(instructions);
  } finally {
    if (createdId !== undefined) await deleteApplication(request, createdId);
    if (importedId !== undefined) await deleteApplication(request, importedId);
  }
});

test('EXP-008: a 5 000-character instruction survives the round trip', async ({ request }) => {
  const name = autotestName('large');
  const sections = Array.from({ length: 30 }, (_unused, index) => {
    const n = index + 1;
    return [
      `## Section ${String(n)}`,
      '',
      `This is section ${String(n)} of the detailed instructions. It contains important ` +
        'guidelines about testing methodology, best practices, and common patterns to ' +
        'follow when writing automated tests for web applications.',
      '',
      `- Guideline ${String(n)}.1: Always validate preconditions`,
      `- Guideline ${String(n)}.2: Use descriptive assertions`,
      `- Guideline ${String(n)}.3: Clean up test data after each run`,
    ].join('\n');
  });
  const instructions = `You are a comprehensive test automation expert.\n\n${sections.join('\n\n')}`;
  expect(instructions.length, 'the fixture must exceed the size the case is about').toBeGreaterThan(5000);

  let createdId: string | undefined;
  let importedId: string | undefined;
  try {
    createdId = await createApplication(request, { name, instructions });

    const exported = await exportMarkdown(request, createdId);
    expect(exported.text.length, 'the export truncated the document').toBeGreaterThan(5000);

    await deleteApplication(request, createdId);
    await expectDeleted(request, createdId);
    createdId = undefined;

    importedId = importedAgentId(await importWizard(request, buildImportPayloadFromExport(exported.parsed)));
    const version = defaultVersionOf(await readApplication(request, importedId));

    expect((version.instructions ?? '').length).toBeGreaterThan(5000);
    expect(normaliseWhitespace(version.instructions ?? '')).toBe(normaliseWhitespace(instructions));
  } finally {
    if (createdId !== undefined) await deleteApplication(request, createdId);
    if (importedId !== undefined) await deleteApplication(request, importedId);
  }
});

test('EXP-009: an agent can be imported from a hand-built payload, with no export', async ({
  request,
}) => {
  const name = autotestName('manual');
  let importedId: string | undefined;
  try {
    // The other half of the contract: the wizard's body is a documented shape,
    // not only whatever this platform's exporter happens to emit. A client
    // that generates agents — a scaffolder, a migration script — sends this.
    const payload = buildImportPayload(name, 'Manually constructed import', {
      instructions: 'You are an agent created from a manual import payload.',
      agentType: 'openai',
      temperature: 0.4,
      maxTokens: 512,
      variables: [{ name: 'mode', value: 'test' }],
    });

    importedId = importedAgentId(await importWizard(request, payload));
    const imported = await readApplication(request, importedId);
    const version = defaultVersionOf(imported);

    expect(imported.name).toBe(name);
    expect(imported.description).toBe('Manually constructed import');
    expect(version.instructions).toBe('You are an agent created from a manual import payload.');
    expect(version.llm_settings?.['temperature']).toBe(0.4);
    expect(version.llm_settings?.['max_tokens']).toBe(512);
    expect(variablesAsRecord(version)).toStrictEqual({ mode: 'test' });
  } finally {
    if (importedId !== undefined) await deleteApplication(request, importedId);
  }
});
