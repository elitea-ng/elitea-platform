/**
 * PEXP: a pipeline survives being exported to a `.pipeline.md` file and
 * imported back through the platform's own API.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT THIS PORTS
 * ─────────────────────────────────────────────────────────────────────────────
 * `EliteaAI/elitea-testing-public`,
 * `automation/tests/api/export_import/test_export_import_pipelines.py` — all
 * ten tests, one journey each, keeping the legacy PEXP-0nn identifiers:
 *
 *   PEXP-001 test_pipeline_export_import_roundtrip_basic
 *   PEXP-002 test_pipeline_export_import_preserves_all_fields
 *   PEXP-003 test_pipeline_export_format_is_valid_yaml
 *   PEXP-004 test_pipeline_import_with_conflicting_name
 *   PEXP-005 test_pipeline_import_preserves_original_when_conflict
 *   PEXP-006 test_pipeline_export_import_empty_description
 *   PEXP-007 test_pipeline_export_import_special_characters_in_name
 *   PEXP-008 test_pipeline_export_import_complex_topology
 *   PEXP-009 test_pipeline_export_import_all_node_types
 *   PEXP-010 test_pipeline_import_via_constructed_payload
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT A PIPELINE'S FILE LOOKS LIKE, AND WHY IT IS ITS OWN SUITE
 * ─────────────────────────────────────────────────────────────────────────────
 * A pipeline is an application whose version carries `agent_type: pipeline` and
 * whose `instructions` are a YAML graph rather than prose. The exporter LIFTS
 * that graph out of the body and into the frontmatter — `entry_point` and
 * `nodes` become top-level keys and the body is empty
 * (`internal/api/v2/eliteacore/export_markdown.go`, the `pipeline` branch) —
 * so the round trip has a whole extra step the agent one does not: the graph is
 * re-serialised on the way back in. A node, a transition or a route lost there
 * is a pipeline that still opens in the editor and runs a different program.
 * Nothing in the agent file exercises that path.
 *
 * The node ids are `LLM_1`, not the legacy suite's `"LLM 1"`: the Rust
 * compiler's `valid_graph_id` refuses a space, so the legacy fixture mints a
 * pipeline that round-trips perfectly and cannot run. See
 * `e2e/fixtures/exportImport.ts` for the full note.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT THIS RUN LEAVES BEHIND
 * ─────────────────────────────────────────────────────────────────────────────
 * Nothing. Each journey creates its own `autotest_*` pipelines and deletes
 * them, including the ones an import created, in a `finally`.
 */
import { test, expect } from '@playwright/test';

import { AUTOTEST_PREFIX } from '../../fixtures/api';
import {
  CODE_NODE,
  DECISION_NODE,
  LLM_NODE,
  PRINTER_NODE,
  ROUTER_NODE,
  SECOND_LLM_NODE,
  applicationIsGone,
  buildImportPayload,
  buildImportPayloadFromExport,
  codeNode,
  createApplication,
  decisionNode,
  defaultVersionOf,
  deleteApplication,
  exportMarkdown,
  importWizard,
  importedAgentId,
  llmNode,
  parsePipelineInstructions,
  pipelineInstructions,
  printerNode,
  readApplication,
  routerNode,
  type PipelineNode,
} from '../../fixtures/exportImport';
import { STORAGE_STATE } from '../../../playwright.config';

test.use({ storageState: STORAGE_STATE.admin });

function autotestName(tag: string): string {
  return `${AUTOTEST_PREFIX}${tag}_${Math.random().toString(36).slice(2, 8)}`;
}

/** See the agents spec: the legacy 0.5 s sleep, replaced by the server read it was standing in for. */
async function expectDeleted(
  request: Parameters<typeof applicationIsGone>[0],
  id: string,
): Promise<void> {
  await expect
    .poll(async () => applicationIsGone(request, id), {
      timeout: 15_000,
      message: `pipeline ${id} is still readable after its DELETE`,
    })
    .toBe(true);
}

interface PipelineFixture {
  readonly name: string;
  readonly description?: string;
  readonly nodes?: readonly PipelineNode[];
  readonly temperature?: number;
  readonly maxTokens?: number;
}

/** Create a pipeline: an application whose version is `agent_type: pipeline` and whose instructions are the graph. */
async function createPipeline(
  request: Parameters<typeof createApplication>[0],
  fixture: PipelineFixture,
): Promise<string> {
  const nodes = fixture.nodes ?? [llmNode(LLM_NODE)];
  return createApplication(request, {
    name: fixture.name,
    description: fixture.description ?? 'Export/import test pipeline',
    agentType: 'pipeline',
    instructions: pipelineInstructions(LLM_NODE, nodes),
    temperature: fixture.temperature ?? 0.6,
    maxTokens: fixture.maxTokens ?? 1024,
  });
}

/** The graph a stored pipeline version holds, parsed. */
function storedGraph(instructions: string | undefined): {
  entry_point?: unknown;
  nodes?: readonly PipelineNode[];
} {
  return parsePipelineInstructions(instructions) as { entry_point?: unknown; nodes?: readonly PipelineNode[] };
}

test('PEXP-001: an exported pipeline imports back as a pipeline with its graph', async ({
  request,
}) => {
  const name = autotestName('pexp_basic');
  let createdId: string | undefined;
  let importedId: string | undefined;
  try {
    createdId = await createPipeline(request, {
      name,
      description: 'Roundtrip test pipeline',
      nodes: [llmNode(LLM_NODE)],
    });

    const exported = await exportMarkdown(request, createdId);
    expect(exported.text.length).toBeGreaterThan(0);
    expect(exported.parsed.frontmatter['name']).toBe(name);
    expect(exported.parsed.frontmatter['agent_type']).toBe('pipeline');
    expect(exported.parsed.frontmatter['entry_point']).toBe(LLM_NODE);
    expect((exported.parsed.frontmatter['nodes'] as unknown[] | undefined)?.length).toBe(1);

    await deleteApplication(request, createdId);
    await expectDeleted(request, createdId);
    createdId = undefined;

    importedId = importedAgentId(await importWizard(request, buildImportPayloadFromExport(exported.parsed)));
    const imported = await readApplication(request, importedId);
    const version = defaultVersionOf(imported);

    expect(imported.name).toBe(name);
    expect(imported.description).toBe('Roundtrip test pipeline');
    // The type is what routes the version to the graph assembler rather than
    // to the chat executor. An import that lost it would leave the same rows
    // running as an agent, with nothing on screen to say so.
    expect(version.agent_type).toBe('pipeline');

    const graph = storedGraph(version.instructions);
    expect(graph.entry_point).toBe(LLM_NODE);
    expect(graph.nodes?.length).toBe(1);
    expect(graph.nodes?.[0]?.['type']).toBe('llm');
  } finally {
    if (createdId !== undefined) await deleteApplication(request, createdId);
    if (importedId !== undefined) await deleteApplication(request, importedId);
  }
});

test('PEXP-002: a two-node pipeline keeps every field and its transition', async ({ request }) => {
  const name = autotestName('pexp_full');
  let createdId: string | undefined;
  let importedId: string | undefined;
  try {
    createdId = await createPipeline(request, {
      name,
      description: 'Full field coverage pipeline',
      nodes: [llmNode(LLM_NODE, PRINTER_NODE), printerNode(PRINTER_NODE)],
      temperature: 0.3,
      maxTokens: 4096,
    });

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
    expect(importedVersion.agent_type).toBe('pipeline');
    expect(importedVersion.llm_settings?.['model_name']).toStrictEqual(
      originalVersion.llm_settings?.['model_name'],
    );
    expect(importedVersion.llm_settings?.['temperature']).toStrictEqual(
      originalVersion.llm_settings?.['temperature'],
    );
    // Structural, not textual: the graph is re-serialised on the way back, so
    // two different byte sequences can be the same program and a string
    // comparison would report the serialiser rather than the data.
    expect(storedGraph(importedVersion.instructions)).toStrictEqual(
      storedGraph(originalVersion.instructions),
    );
  } finally {
    if (createdId !== undefined) await deleteApplication(request, createdId);
    if (importedId !== undefined) await deleteApplication(request, importedId);
  }
});

test('PEXP-003: a pipeline export carries the graph in the frontmatter and an empty body', async ({
  request,
}) => {
  const name = autotestName('pexp_fmt');
  let createdId: string | undefined;
  try {
    createdId = await createPipeline(request, { name, nodes: [llmNode(LLM_NODE)] });

    const exported = await exportMarkdown(request, createdId);
    expect(exported.text.startsWith('---\n')).toBe(true);
    expect(Object.keys(exported.parsed.frontmatter)).toEqual(
      expect.arrayContaining(['name', 'agent_type', 'entry_point', 'nodes']),
    );
    expect(exported.parsed.frontmatter['agent_type']).toBe('pipeline');
    // The half that distinguishes this format from the agent one: a pipeline
    // whose graph leaked into the body would be imported as prose
    // instructions and would compile to nothing.
    expect(exported.parsed.body).toBe('');
  } finally {
    if (createdId !== undefined) await deleteApplication(request, createdId);
  }
});

test('PEXP-004: importing over a pipeline name that already exists creates a second one', async ({
  request,
}) => {
  const name = autotestName('pexp_conflict');
  let originalId: string | undefined;
  let importedId: string | undefined;
  try {
    originalId = await createPipeline(request, {
      name,
      description: 'Original pipeline',
      nodes: [llmNode(LLM_NODE)],
    });

    const exported = await exportMarkdown(request, originalId);
    importedId = importedAgentId(await importWizard(request, buildImportPayloadFromExport(exported.parsed)));

    expect(importedId, 'the import overwrote the existing pipeline instead of adding one').not.toBe(
      originalId,
    );
    expect(defaultVersionOf(await readApplication(request, importedId)).agent_type).toBe('pipeline');
  } finally {
    if (originalId !== undefined) await deleteApplication(request, originalId);
    if (importedId !== undefined) await deleteApplication(request, importedId);
  }
});

test('PEXP-005: the existing pipeline is untouched by a colliding import', async ({ request }) => {
  const name = autotestName('pexp_preserve');
  let originalId: string | undefined;
  let importedId: string | undefined;
  try {
    originalId = await createPipeline(request, {
      name,
      description: 'Original description',
      nodes: [llmNode(LLM_NODE)],
      temperature: 0.3,
    });
    const before = await readApplication(request, originalId);

    const exported = await exportMarkdown(request, originalId);
    importedId = importedAgentId(await importWizard(request, buildImportPayloadFromExport(exported.parsed)));

    const after = await readApplication(request, originalId);
    expect(after.name).toBe(before.name);
    expect(after.description).toBe(before.description);
    expect(defaultVersionOf(after).instructions).toBe(defaultVersionOf(before).instructions);
    expect(defaultVersionOf(after).agent_type).toBe('pipeline');
  } finally {
    if (originalId !== undefined) await deleteApplication(request, originalId);
    if (importedId !== undefined) await deleteApplication(request, importedId);
  }
});

test('PEXP-006: a pipeline with an empty description round-trips as empty', async ({ request }) => {
  const name = autotestName('pexp_empty');
  let createdId: string | undefined;
  let importedId: string | undefined;
  try {
    createdId = await createPipeline(request, { name, description: '', nodes: [llmNode(LLM_NODE)] });

    const exported = await exportMarkdown(request, createdId);
    await deleteApplication(request, createdId);
    await expectDeleted(request, createdId);
    createdId = undefined;

    importedId = importedAgentId(await importWizard(request, buildImportPayloadFromExport(exported.parsed)));
    const imported = await readApplication(request, importedId);

    expect(imported.name).toBe(name);
    expect(imported.description ?? '').toBe('');
    expect(defaultVersionOf(imported).agent_type).toBe('pipeline');
  } finally {
    if (createdId !== undefined) await deleteApplication(request, createdId);
    if (importedId !== undefined) await deleteApplication(request, importedId);
  }
});

test('PEXP-007: special characters and unicode survive a pipeline round trip', async ({
  request,
}) => {
  const name = `${AUTOTEST_PREFIX}p&p@-${Math.random().toString(36).slice(2, 6)}`;
  const description = `Pipeline with special chars: <>&"' — ключи, 日本語, 😀`;
  let createdId: string | undefined;
  let importedId: string | undefined;
  try {
    createdId = await createPipeline(request, { name, description, nodes: [llmNode(LLM_NODE)] });

    const exported = await exportMarkdown(request, createdId);
    await deleteApplication(request, createdId);
    await expectDeleted(request, createdId);
    createdId = undefined;

    importedId = importedAgentId(await importWizard(request, buildImportPayloadFromExport(exported.parsed)));
    const imported = await readApplication(request, importedId);

    expect(imported.name).toBe(name);
    expect(imported.description).toBe(description);
  } finally {
    if (createdId !== undefined) await deleteApplication(request, createdId);
    if (importedId !== undefined) await deleteApplication(request, importedId);
  }
});

test('PEXP-008: a branching topology survives with every node and its transitions', async ({
  request,
}) => {
  const name = autotestName('pexp_complex');
  let createdId: string | undefined;
  let importedId: string | undefined;
  try {
    // LLM -> Decision -> (LLM_2 | Printer) -> END. The branch is the point:
    // a serialiser that flattened `transitions` would keep all four nodes and
    // lose the program.
    const nodes = [
      llmNode(LLM_NODE, DECISION_NODE),
      decisionNode(DECISION_NODE, { true: SECOND_LLM_NODE, false: PRINTER_NODE }),
      llmNode(SECOND_LLM_NODE),
      printerNode(PRINTER_NODE),
    ];
    createdId = await createPipeline(request, { name, description: 'Complex topology pipeline', nodes });
    const originalVersion = defaultVersionOf(await readApplication(request, createdId));

    const exported = await exportMarkdown(request, createdId);
    await deleteApplication(request, createdId);
    await expectDeleted(request, createdId);
    createdId = undefined;

    importedId = importedAgentId(await importWizard(request, buildImportPayloadFromExport(exported.parsed)));
    const importedVersion = defaultVersionOf(await readApplication(request, importedId));
    const graph = storedGraph(importedVersion.instructions);

    expect(graph.entry_point).toBe(LLM_NODE);
    expect(graph.nodes?.length).toBe(4);
    expect(new Set((graph.nodes ?? []).map((node) => node['id']))).toStrictEqual(
      new Set([LLM_NODE, DECISION_NODE, SECOND_LLM_NODE, PRINTER_NODE]),
    );
    const typeById = Object.fromEntries((graph.nodes ?? []).map((node) => [node['id'], node['type']]));
    expect(typeById).toStrictEqual({
      [LLM_NODE]: 'llm',
      [DECISION_NODE]: 'decision',
      [SECOND_LLM_NODE]: 'llm',
      [PRINTER_NODE]: 'printer',
    });
    expect(graph).toStrictEqual(storedGraph(originalVersion.instructions));
  } finally {
    if (createdId !== undefined) await deleteApplication(request, createdId);
    if (importedId !== undefined) await deleteApplication(request, importedId);
  }
});

test('PEXP-009: every node type survives the round trip', async ({ request }) => {
  const name = autotestName('pexp_allnodes');
  let createdId: string | undefined;
  let importedId: string | undefined;
  try {
    const nodes = [
      llmNode(LLM_NODE, CODE_NODE),
      codeNode(CODE_NODE, DECISION_NODE),
      decisionNode(DECISION_NODE, { true: ROUTER_NODE, false: PRINTER_NODE }),
      routerNode(ROUTER_NODE, [{ condition: 'default', transition: PRINTER_NODE }]),
      printerNode(PRINTER_NODE),
    ];
    createdId = await createPipeline(request, { name, description: 'All node types pipeline', nodes });
    const originalVersion = defaultVersionOf(await readApplication(request, createdId));

    const exported = await exportMarkdown(request, createdId);
    await deleteApplication(request, createdId);
    await expectDeleted(request, createdId);
    createdId = undefined;

    importedId = importedAgentId(await importWizard(request, buildImportPayloadFromExport(exported.parsed)));
    const importedVersion = defaultVersionOf(await readApplication(request, importedId));
    const graph = storedGraph(importedVersion.instructions);

    expect(graph.nodes?.length).toBe(5);
    // Each node type carries a DIFFERENT shape — `transitions` on a decision,
    // `routes` on a router, `input_mapping.code` on a code node — so this is
    // the case that says the serialiser is generic rather than tuned to the
    // one node the other cases use.
    expect(new Set((graph.nodes ?? []).map((node) => node['type']))).toStrictEqual(
      new Set(['llm', 'code', 'decision', 'router', 'printer']),
    );
    expect(graph).toStrictEqual(storedGraph(originalVersion.instructions));
  } finally {
    if (createdId !== undefined) await deleteApplication(request, createdId);
    if (importedId !== undefined) await deleteApplication(request, importedId);
  }
});

test('PEXP-010: a pipeline can be imported from a hand-built payload, with no export', async ({
  request,
}) => {
  const name = autotestName('pexp_manual');
  let importedId: string | undefined;
  try {
    const payload = buildImportPayload(name, 'Manually constructed pipeline import', {
      instructions: pipelineInstructions(LLM_NODE, [
        llmNode(LLM_NODE, PRINTER_NODE),
        printerNode(PRINTER_NODE),
      ]),
      // `pipeline`, and it is the only thing in the body that says so — the
      // wizard's `entity` is `agents` for both kinds.
      agentType: 'pipeline',
      temperature: 0.4,
      maxTokens: 2048,
    });

    importedId = importedAgentId(await importWizard(request, payload));
    const imported = await readApplication(request, importedId);
    const version = defaultVersionOf(imported);

    expect(imported.name).toBe(name);
    expect(imported.description).toBe('Manually constructed pipeline import');
    expect(version.agent_type).toBe('pipeline');
    expect(version.llm_settings?.['temperature']).toBe(0.4);
    expect(version.llm_settings?.['max_tokens']).toBe(2048);

    const graph = storedGraph(version.instructions);
    expect(graph.entry_point).toBe(LLM_NODE);
    expect(graph.nodes?.length).toBe(2);
  } finally {
    if (importedId !== undefined) await deleteApplication(request, importedId);
  }
});
