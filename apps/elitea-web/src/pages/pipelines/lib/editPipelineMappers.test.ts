import { describe, expect, it } from 'vitest';

import type { ApplicationDetail, ApplicationVersionDetail, ApplicationVersionSummary } from '@/shared/api/generated/model';

import {
  pipelineDetailDisplayName,
  toFormValues,
  toNewPipelineVersionBody,
  toVersionDraft,
  toVersionOptions,
  toVersionSummaries,
} from './editPipelineMappers';

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

describe('toVersionDraft', () => {
  it('carries over name/instructions/variables/tools and the new conversation starters, forcing agentType to "pipeline"', () => {
    const version = {
      name: 'base',
      instructions: 'Be helpful.',
      variables: [{ name: 'x', value: '1' }],
      tools: [{ id: 't1' }],
      tags: [{ name: 'sales' }, { name: null }],
      meta: { step_limit: 40, internal_tools: ['internal_mcp', 'other'] },
    } as unknown as ApplicationVersionDetail;

    expect(toVersionDraft(version, ['Hi there'])).toEqual({
      name: 'base',
      agentType: 'pipeline',
      instructions: 'Be helpful.',
      conversationStarters: ['Hi there'],
      variables: [{ name: 'x', value: '1' }],
      meta: { step_limit: 40, internal_tools: ['internal_mcp', 'other'] },
      tags: ['sales'],
      tools: [{ id: 't1' }],
      pipelineSettings: undefined,
    });
  });

  // #135: `pipelineSettings` used to be hardcoded `undefined` and
  // `instructions` always came from the STORED version, so a graph edit could
  // not reach the wire even in principle.
  it('prefers the live graph draft for instructions and carries its pipelineSettings', () => {
    const version = {
      name: 'base',
      instructions: 'stale yaml',
      meta: {},
    } as unknown as ApplicationVersionDetail;
    const graph = {
      instructions: 'entry_point: Agent 1\n',
      admission: {
        document: {},
        parseFailed: false,
        issues: [],
        hasGraph: true,
        isAdmissible: true,
      },
      pipelineSettings: {
        nodes: [{ id: 'Agent 1' }],
        edges: [],
        orientation: 'vertical',
        layout_version: '1.0',
      },
    };

    const draft = toVersionDraft(version, [], graph);

    expect(draft.instructions).toBe('entry_point: Agent 1\n');
    expect(draft.pipelineSettings).toEqual(graph.pipelineSettings);
  });

  it('keeps the stored instructions and leaves pipelineSettings undefined when no graph draft is supplied', () => {
    const version = {
      name: 'base',
      instructions: 'stale yaml',
      meta: {},
    } as unknown as ApplicationVersionDetail;

    const draft = toVersionDraft(version, [], undefined);

    expect(draft.instructions).toBe('stale yaml');
    expect(draft.pipelineSettings).toBeUndefined();
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
    const version = {
      name: 'base',
      meta: {},
    } as unknown as ApplicationVersionDetail;
    const draft = toVersionDraft(version, []);
    expect(draft.meta).toEqual({ step_limit: 25, internal_tools: [] });
  });

  it('falls back to an EMPTY meta.internal_tools when the stored value is not an array', () => {
    const version = {
      name: 'base',
      meta: { internal_tools: 'internal_mcp' },
    } as unknown as ApplicationVersionDetail;
    expect(toVersionDraft(version, []).meta.internal_tools).toEqual([]);
  });

  // The other direction, and the reason the fallback is the ONLY thing that
  // changed: a user who deliberately turned Elitea MCP Tools on must still
  // find it on when the editor reloads their version.
  it('round-trips an explicitly stored internal_mcp unchanged', () => {
    const version = {
      name: 'base',
      meta: { internal_tools: ['internal_mcp'] },
    } as unknown as ApplicationVersionDetail;
    expect(toVersionDraft(version, []).meta.internal_tools).toEqual(['internal_mcp']);
  });

  it('defaults instructions/variables/tags/tools to empty when absent', () => {
    const version = { name: 'base' } as ApplicationVersionDetail;
    const draft = toVersionDraft(version, []);
    expect(draft.instructions).toBe('');
    expect(draft.variables).toEqual([]);
    expect(draft.tags).toEqual([]);
    expect(draft.tools).toEqual([]);
  });

  it('uses the live form tags so MCP exposure edits reach the save request', () => {
    const version = {
      name: 'base',
      tags: [{ name: 'stored' }],
    } as unknown as ApplicationVersionDetail;

    expect(toVersionDraft(version, [], undefined, undefined, ['stored', 'mcp']).tags).toEqual(['stored', 'mcp']);
  });

  it('always sets agentType to "pipeline", regardless of the wire agent_type', () => {
    const version = {
      name: 'base',
      agent_type: 'classic',
    } as unknown as ApplicationVersionDetail;
    expect(toVersionDraft(version, []).agentType).toBe('pipeline');
  });

  it('always sets pipelineSettings to undefined (disclosed gap, not a fabricated value)', () => {
    const version = { name: 'base' } as ApplicationVersionDetail;
    expect(toVersionDraft(version, []).pipelineSettings).toBeUndefined();
  });

  it('reads the stored llm_settings back, with model_project_id as a number', () => {
    const version = {
      name: 'base',
      llm_settings: {
        model_name: 'qwen3.5',
        model_project_id: '17',
        max_tokens: -1,
        temperature: 0.6,
      },
    } as unknown as ApplicationVersionDetail;

    expect(toVersionDraft(version, []).llmSettings).toEqual({
      model_name: 'qwen3.5',
      model_project_id: 17,
      max_tokens: -1,
      temperature: 0.6,
    });
  });

  // Same edit-wins-over-stored rule the agents twin applies in
  // `toVersionWriteBody`: the page's model picker holds the live choice and a
  // save that re-read the stored blob would drop it.
  it('prefers the picked llm_settings over the stored one', () => {
    const version = {
      name: 'base',
      llm_settings: {
        model_name: 'gpt-4o',
        model_project_id: 3,
        max_tokens: 4096,
      },
    } as unknown as ApplicationVersionDetail;

    const draft = toVersionDraft(version, [], undefined, {
      model_name: 'qwen3.5',
      model_project_id: 17,
      max_tokens: -1,
    });
    expect(draft.llmSettings).toEqual({
      model_name: 'qwen3.5',
      model_project_id: 17,
      max_tokens: -1,
    });
  });

  // `{}` is what every version written before the picker existed stores;
  // `toVersionWriteRequest` then omits the key and the pipeline keeps running
  // on the project's catalogue default.
  it('leaves llmSettings undefined for a version that names no model', () => {
    const version = {
      name: 'base',
      llm_settings: {},
    } as unknown as ApplicationVersionDetail;
    expect(toVersionDraft(version, []).llmSettings).toBeUndefined();
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
    const odd = {
      ...storedVersion,
      agent_type: 'openai',
    } as unknown as ApplicationVersionDetail;
    expect(toNewPipelineVersionBody(odd, [], undefined).agent_type).toBe('pipeline');

    const blank = {
      ...storedVersion,
      agent_type: undefined,
    } as ApplicationVersionDetail;
    expect(toNewPipelineVersionBody(blank, [], undefined).agent_type).toBe('pipeline');
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
    const body = toNewPipelineVersionBody(storedVersion, [], undefined);
    expect(body.meta).toEqual({
      step_limit: 40,
      internal_tools: ['internal_mcp'],
    });
  });

  it('defaults step_limit to 25 and internal_tools to [] for a version with no meta', () => {
    const bare = {
      ...storedVersion,
      meta: undefined,
    } as unknown as ApplicationVersionDetail;
    expect(toNewPipelineVersionBody(bare, [], undefined).meta).toEqual({
      step_limit: 25,
      internal_tools: [],
    });
  });

  // Same edit-wins-over-stored rule the ordinary Save applies: the live
  // starters come off the form, not off the server's last-saved copy.
  it('clones the LIVE conversation starters, not the stored ones', () => {
    const withStored = {
      ...storedVersion,
      conversation_starters: ['old'],
    } as unknown as ApplicationVersionDetail;
    expect(toNewPipelineVersionBody(withStored, ['typed but unsaved'], undefined).conversation_starters).toEqual(['typed but unsaved']);
  });

  it('prefers the picked model over the stored blob, and forwards the stored one verbatim with no pick', () => {
    const withModel = {
      ...storedVersion,
      llm_settings: { model_name: 'gpt-4o', model_project_id: 3 },
    } as unknown as ApplicationVersionDetail;

    expect(
      toNewPipelineVersionBody(withModel, [], {
        model_name: 'qwen3.5',
        model_project_id: 17,
        max_tokens: -1,
      }).llm_settings,
    ).toEqual({ model_name: 'qwen3.5', model_project_id: 17, max_tokens: -1 });
    expect(toNewPipelineVersionBody(withModel, [], undefined).llm_settings).toEqual({
      model_name: 'gpt-4o',
      model_project_id: 3,
    });
  });

  it('omits llm_settings entirely for a version that names no model', () => {
    expect(toNewPipelineVersionBody(storedVersion, [], undefined)).not.toHaveProperty('llm_settings');
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

    expect(toNewPipelineVersionBody(rich, [], undefined).meta).toEqual({
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

    const body = toNewPipelineVersionBody(withMetaVariables, [], undefined);

    expect(body.meta).not.toHaveProperty('variables');
    expect(body.variables).toEqual([]);
  });
});
