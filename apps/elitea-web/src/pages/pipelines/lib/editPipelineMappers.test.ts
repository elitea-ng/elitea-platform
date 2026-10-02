import { describe, expect, it } from 'vitest';

import type { ApplicationDetail, ApplicationVersionDetail, ApplicationVersionSummary } from '@/shared/api/generated/model';

import {
  pipelineDetailDisplayName,
  toFormValues,
  toNewPipelineVersionBody,
  toPipelineVersionSaveBody,
  toVersionOptions,
  toVersionSummaries,
} from './editPipelineMappers';
import type { EditPipelineVersionFields } from './useEditPipelineVersionFields';

/** The version-level form state a save reads, with every field at its "nothing typed yet" value. */
const NO_EDITS: EditPipelineVersionFields = {
  welcomeMessage: '',
  variables: [],
  stepLimit: undefined,
  internalTools: [],
  llmSettings: undefined,
  tags: [],
};

function edits(overrides: Partial<EditPipelineVersionFields>): EditPipelineVersionFields {
  return { ...NO_EDITS, ...overrides };
}

describe('toVersionSummaries', () => {
  it('maps snake_case fields to the camelCase VersionSummary shape', () => {
    const wire: readonly ApplicationVersionSummary[] = [
      {
        id: '1',
        name: 'base',
        status: 'draft',
        agent_type: 'pipeline',
        created_at: '2026-01-01T00:00:00Z',
      },
    ];
    expect(toVersionSummaries(wire)).toEqual([
      {
        id: '1',
        name: 'base',
        status: 'draft',
        agentType: 'pipeline',
        createdAt: '2026-01-01T00:00:00Z',
      },
    ]);
  });

  it('maps an empty list to an empty list', () => {
    expect(toVersionSummaries([])).toEqual([]);
  });
});

describe('pipelineDetailDisplayName', () => {
  it('returns the trimmed name when non-blank', () => {
    const detail = { name: 'My Pipeline' } as ApplicationDetail;
    expect(pipelineDetailDisplayName(detail)).toBe('My Pipeline');
  });

  it('falls back to "Untitled" for a blank name', () => {
    const detail = { name: '   ' } as ApplicationDetail;
    expect(pipelineDetailDisplayName(detail)).toBe('Untitled');
  });
});

describe('toFormValues', () => {
  it('seeds name/description from the detail and conversation_starters from the version', () => {
    const detail = {
      name: 'My Pipeline',
      description: 'A helpful pipeline',
    } as ApplicationDetail;
    const version = {
      conversation_starters: ['Hi', null, undefined, 'Bye'],
      tags: [{ name: 'mcp' }, { name: null }],
    } as unknown as ApplicationVersionDetail;
    expect(toFormValues(detail, version)).toEqual({
      name: 'My Pipeline',
      description: 'A helpful pipeline',
      version_details: { conversation_starters: ['Hi', 'Bye'], tags: ['mcp'] },
    });
  });

  it('defaults conversation_starters to [] when there is no version yet', () => {
    const detail = {
      name: 'My Pipeline',
      description: 'A helpful pipeline',
    } as ApplicationDetail;
    expect(toFormValues(detail, undefined)).toEqual({
      name: 'My Pipeline',
      description: 'A helpful pipeline',
      version_details: { conversation_starters: [], tags: [] },
    });
  });
});

describe('toPipelineVersionSaveBody', () => {
  const storedVersion = {
    name: 'base',
    agent_type: 'pipeline',
    instructions: 'Be helpful.',
    welcome_message: 'stored hello',
    variables: [{ name: 'x', value: '1' }],
    tools: [{ id: 't1' }],
    tags: [{ id: 4, name: 'sales' }],
    meta: { step_limit: 40, internal_tools: ['internal_mcp'] },
  } as unknown as ApplicationVersionDetail;

  it('sends the ten keys UpdateVersion writes, and no key it drops', () => {
    const body = toPipelineVersionSaveBody(
      storedVersion,
      ['Hi there'],
      edits({ welcomeMessage: 'live hello', variables: [{ name: 'x', value: '2' }], stepLimit: 12, internalTools: ['attachments'], tags: [{ id: 4, name: 'sales', data: null }] }),
    );

    expect(body).toEqual({
      name: 'base',
      agent_type: 'pipeline',
      instructions: 'Be helpful.',
      welcome_message: 'live hello',
      conversation_starters: ['Hi there'],
      variables: [{ name: 'x', value: '2' }],
      meta: { step_limit: 12, internal_tools: ['attachments'] },
      tags: [{ id: 4, name: 'sales' }],
    });
    // `tools` is accepted by the schema and read by no branch of the handler.
    expect(body).not.toHaveProperty('tools');
    // `notes` has no column, no schema property and no handler branch.
    expect(body).not.toHaveProperty('notes');
  });

  /**
   * The defect this mapper replaced `toVersionDraft` for: the old draft path
   * had no `tags` key at all, so a tag typed into the configuration form was
   * rendered from the server response and dropped on save.
   */
  it('always sends tags, and strips the placeholder id a just-typed tag carries', () => {
    const body = toPipelineVersionSaveBody(storedVersion, [], edits({ tags: [{ id: 0, name: 'new-tag', data: null }] }));
    expect(body.tags).toEqual([{ name: 'new-tag' }]);

    // Removing the LAST tag has to reach the wire: `UpdateVersion` reads the
    // key's presence, so an empty array clears the stored set.
    expect(toPipelineVersionSaveBody(storedVersion, [], NO_EDITS).tags).toEqual([]);
  });

  it('sends the LIVE welcome message, not the stored one', () => {
    expect(toPipelineVersionSaveBody(storedVersion, [], edits({ welcomeMessage: 'typed' })).welcome_message).toBe('typed');
  });

  // #135: `pipeline_settings` used to be absent and `instructions` always came
  // from the STORED version, so a graph edit could not reach the wire.
  it('prefers the live graph draft for instructions and carries its pipeline_settings', () => {
    const version = { name: 'base', instructions: 'stale yaml', meta: {} } as unknown as ApplicationVersionDetail;
    const graph = {
      instructions: 'entry_point: Agent_1\n',
      admission: { document: {}, parseFailed: false, issues: [], hasGraph: true, isAdmissible: true },
      pipelineSettings: { nodes: [{ id: 'Agent_1' }], edges: [], orientation: 'vertical', layout_version: '1.0' },
    };

    const body = toPipelineVersionSaveBody(version, [], NO_EDITS, graph);

    expect(body.instructions).toBe('entry_point: Agent_1\n');
    expect(body.pipeline_settings).toEqual(graph.pipelineSettings);
  });

  it('keeps the stored instructions and omits pipeline_settings entirely when no graph draft is supplied', () => {
    const version = { name: 'base', instructions: 'stale yaml', meta: {} } as unknown as ApplicationVersionDetail;
    const body = toPipelineVersionSaveBody(version, [], NO_EDITS);
    expect(body.instructions).toBe('stale yaml');
    expect(body).not.toHaveProperty('pipeline_settings');
  });

  /*
   * The `internal_tools` fallback used to be `['internal_mcp']`, which the
   * chat query refuses: it admits a version only when
   * `COALESCE(meta -> 'internal_tools', '[]') IN ('[]', '["ask_user"]')`
   * (`services/elitea-main/internal/db/queries/agent_chat.sql:359-362`), so a
   * stored version with NO `internal_tools` key — which answers turns fine
   * today, thanks to that COALESCE — was quietly given one the first time a
   * user saved any unrelated edit, and stopped answering with a 422.
   */
  it('falls back to an EMPTY meta.internal_tools (never internal_mcp) when the existing meta lacks the key', () => {
    const version = { name: 'base', meta: {} } as unknown as ApplicationVersionDetail;
    expect(toPipelineVersionSaveBody(version, [], NO_EDITS).meta).toEqual({ step_limit: 25, internal_tools: [] });
  });

  it('merges over the stored meta so icon_meta and friends survive a save', () => {
    const rich = {
      name: 'base',
      meta: { step_limit: 40, internal_tools: ['internal_mcp'], icon_meta: { id: 9 }, category: 'ops' },
    } as unknown as ApplicationVersionDetail;

    expect(toPipelineVersionSaveBody(rich, [], edits({ stepLimit: 40, internalTools: ['internal_mcp'] })).meta).toEqual({
      step_limit: 40,
      internal_tools: ['internal_mcp'],
      icon_meta: { id: 9 },
      category: 'ops',
    });
  });

  /**
   * `variables` is the one stored-meta key that must NOT be forwarded: both
   * handlers rebuild `meta.variables` from the body's TOP-LEVEL list, and on
   * the create path the carried copy WINS. Forwarding it resurrected deleted
   * variables — secrets among them.
   */
  it('drops meta.variables, which the handler rebuilds from the top-level list', () => {
    const withMetaVariables = {
      name: 'base',
      meta: { step_limit: 40, internal_tools: [], variables: [{ name: 'deleted_secret', value: 'hunter2' }] },
    } as unknown as ApplicationVersionDetail;

    const body = toPipelineVersionSaveBody(withMetaVariables, [], NO_EDITS);

    expect(body.meta).not.toHaveProperty('variables');
    expect(body.variables).toEqual([]);
  });

  it('always pins agent_type to "pipeline", regardless of the wire agent_type', () => {
    const version = { name: 'base', agent_type: 'openai' } as unknown as ApplicationVersionDetail;
    expect(toPipelineVersionSaveBody(version, [], NO_EDITS).agent_type).toBe('pipeline');
  });

  it('prefers the picked llm_settings over the stored one and forwards the stored blob verbatim with no pick', () => {
    const version = {
      name: 'base',
      llm_settings: {
        model_name: 'gpt-4o',
        model_project_id: 3,
        max_tokens: 4096,
      },
    } as unknown as ApplicationVersionDetail;

    expect(
      toPipelineVersionSaveBody(version, [], edits({ llmSettings: { model_name: 'qwen3.5', model_project_id: 17, max_tokens: -1 } })).llm_settings,
    ).toEqual({ model_name: 'qwen3.5', model_project_id: 17, max_tokens: -1 });

    expect(toPipelineVersionSaveBody(version, [], NO_EDITS).llm_settings).toEqual({
      model_name: 'gpt-4o',
      model_project_id: 3,
      max_tokens: 4096,
    });
  });

  it('omits llm_settings entirely for a version that names no model', () => {
    const version = { name: 'base' } as ApplicationVersionDetail;
    expect(toPipelineVersionSaveBody(version, [], NO_EDITS)).not.toHaveProperty('llm_settings');
  });
});

describe('toVersionOptions', () => {
  // The selector compares its option id against `applicationVersionId` with
  // `===`; the wire sends "numeric id serialized as string", so a string on
  // one side means the selected tick never renders and the trigger falls back
  // to the first option's label.
  it('narrows the wire id to a number', () => {
    const wire: readonly ApplicationVersionSummary[] = [
      {
        id: '7',
        name: 'v1',
        status: 'draft',
        agent_type: 'pipeline',
        created_at: '2026-02-01T00:00:00Z',
      },
    ];
    const [option] = toVersionOptions(wire);
    expect(option?.id).toBe(7);
    expect(typeof option?.id).toBe('number');
    expect(option?.name).toBe('v1');
    // Absent on the wire reads as "not the default", never as undefined.
    expect(option?.is_default).toBe(false);
  });

  it("carries the server's default-version flag through", () => {
    const wire = [
      {
        id: '7',
        name: 'v1',
        status: 'draft',
        agent_type: 'pipeline',
        created_at: '2026-02-01T00:00:00Z',
        is_default: true,
      },
    ] as unknown as readonly ApplicationVersionSummary[];
    expect(toVersionOptions(wire)[0]?.is_default).toBe(true);
  });
});

describe('toNewPipelineVersionBody', () => {
  const storedVersion = {
    id: '1',
    name: 'base',
    agent_type: 'pipeline',
    instructions: 'entry_point: LLM_1\nnodes: []\n',
    welcome_message: 'hi',
    variables: [{ name: 'k', value: 'v' }],
    meta: { step_limit: 40, internal_tools: ['internal_mcp'] },
  } as unknown as ApplicationVersionDetail;

  /**
   * The load-bearing one. `insertVersion` substitutes `defaultAgentType` —
   * the literal `"openai"` (`internal/infra/db/repos/applications.go:29,
   * 493-496`) — for an empty `agent_type`, so a body that cloned a blank or
   * absent value would turn a pipeline into an OPENAI AGENT on the way to the
   * new version: same rows, wrong executor, and nothing on screen to say so.
   */
  it('pins agent_type to pipeline even when the stored version names something else', () => {
    const odd = { ...storedVersion, agent_type: 'openai' } as unknown as ApplicationVersionDetail;
    expect(toNewPipelineVersionBody(odd, [], NO_EDITS).agent_type).toBe('pipeline');

    const blank = { ...storedVersion, agent_type: undefined } as ApplicationVersionDetail;
    expect(toNewPipelineVersionBody(blank, [], NO_EDITS).agent_type).toBe('pipeline');
  });

  /**
   * `versionFromBody` DOES read `meta` off the create body and only defaults
   * `step_limit` when the caller sent none (`applications/handler.go:504-510`)
   * — contradicting `features/agents/model/useSaveNewVersion.ts`'s doc
   * comment, which still says the handler ignores the key. Omitting it here
   * would silently reset this pipeline's step limit to 25 and drop its
   * internal tools on every save-as-version.
   */
  it('carries meta.step_limit and meta.internal_tools onto the new version', () => {
    const body = toNewPipelineVersionBody(storedVersion, [], edits({ stepLimit: 40, internalTools: ['internal_mcp'] }));
    expect(body.meta).toEqual({ step_limit: 40, internal_tools: ['internal_mcp'] });
  });

  it('defaults step_limit to 25 and internal_tools to [] for a version with no meta', () => {
    const bare = { ...storedVersion, meta: undefined } as unknown as ApplicationVersionDetail;
    expect(toNewPipelineVersionBody(bare, [], NO_EDITS).meta).toEqual({ step_limit: 25, internal_tools: [] });
  });

  // Same edit-wins-over-stored rule the ordinary Save applies: the live
  // starters come off the form, not off the server's last-saved copy.
  it('clones the LIVE conversation starters, not the stored ones', () => {
    const withStored = { ...storedVersion, conversation_starters: ['old'] } as unknown as ApplicationVersionDetail;
    expect(toNewPipelineVersionBody(withStored, ['typed but unsaved'], NO_EDITS).conversation_starters).toEqual([
      'typed but unsaved',
    ]);
  });

  it('prefers the picked model over the stored blob, and forwards the stored one verbatim with no pick', () => {
    const withModel = {
      ...storedVersion,
      llm_settings: { model_name: 'gpt-4o', model_project_id: 3 },
    } as unknown as ApplicationVersionDetail;

    expect(toNewPipelineVersionBody(withModel, [], edits({ llmSettings: { model_name: 'qwen3.5', model_project_id: 17, max_tokens: -1 } })).llm_settings).toEqual(
      { model_name: 'qwen3.5', model_project_id: 17, max_tokens: -1 },
    );
    expect(toNewPipelineVersionBody(withModel, [], NO_EDITS).llm_settings).toEqual({
      model_name: 'gpt-4o',
      model_project_id: 3,
    });
  });

  it('omits llm_settings entirely for a version that names no model', () => {
    expect(toNewPipelineVersionBody(storedVersion, [], edits({ stepLimit: 40, internalTools: ['internal_mcp'] }))).not.toHaveProperty('llm_settings');
  });

  /**
   * `versionFromBody` takes `vBody["meta"]` as the WHOLE map and
   * `insertVersion` persists it verbatim, so anything this mapper does not
   * re-send is gone from the clone — permanently, since nothing writes it
   * back. `icon_meta` is the measurable one: `toChatPipelineVersionDetails`,
   * in this same file, reads it off a pipeline version's `meta` and forwards
   * it to the chat.
   */
  it('merges over the stored meta instead of replacing it, so icon_meta and friends survive the clone', () => {
    const rich = {
      ...storedVersion,
      meta: {
        step_limit: 40,
        internal_tools: ['internal_mcp'],
        icon_meta: { id: 9, name: 'robot.png' },
        category: 'ops',
        attachment_storage: 'artifacts',
      },
    } as unknown as ApplicationVersionDetail;

    expect(toNewPipelineVersionBody(rich, [], edits({ stepLimit: 40, internalTools: ['internal_mcp'] })).meta).toEqual({
      step_limit: 40,
      internal_tools: ['internal_mcp'],
      icon_meta: { id: 9, name: 'robot.png' },
      category: 'ops',
      attachment_storage: 'artifacts',
    });
  });

  /**
   * `variables` is the one stored-meta key that must NOT be forwarded, and the
   * agents twin (`editApplicationMappers.ts`'s `toVersionMetaBody`) makes the
   * same cut for a measured reason: both handlers rebuild `meta.variables`
   * from the body's TOP-LEVEL list, and on the create path the carried copy
   * WINS, because `versionFromBody` folds the list only when it is non-empty
   * (`applications/handler.go:509-511`). Forwarding it resurrected deleted
   * variables — secrets among them — into every turn of the cloned version.
   */
  it('drops meta.variables, which the handler rebuilds from the top-level list', () => {
    const withMetaVariables = {
      ...storedVersion,
      variables: [],
      meta: {
        step_limit: 40,
        internal_tools: [],
        variables: [{ name: 'deleted_secret', value: 'hunter2' }],
      },
    } as unknown as ApplicationVersionDetail;

    const body = toNewPipelineVersionBody(withMetaVariables, [], NO_EDITS);

    expect(body.meta).not.toHaveProperty('variables');
    expect(body.variables).toEqual([]);
  });
});
