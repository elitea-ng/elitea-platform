import { describe, expect, it } from 'vitest';

import type { ApplicationDetail, ApplicationVersionDetail, ApplicationVersionSummary } from '@/shared/api/generated/model';
import { VersionWriteRequest } from '@/shared/api/generated/model/versionWriteRequest.zod';

import type { EditApplicationVersionFields } from './useEditApplicationVersionFields';
import {
  applicationDetailDisplayName,
  toFormValues,
  toVersionDraft,
  toVersionOptions,
  toVersionSaveBody,
  toVersionSummaries,
  toVersionWriteBody,
} from './editApplicationMappers';

describe('toVersionOptions', () => {
  it('narrows the wire\'s string ids to numbers so the selector can match the active version', () => {
    const wire: readonly ApplicationVersionSummary[] = [
      { id: '7', name: 'base', status: 'draft', agent_type: 'classic', created_at: '2026-01-01T00:00:00Z' },
    ];
    expect(toVersionOptions(wire)).toEqual([
      // `is_default` is false, not absent: the wire row above carries no such
      // key, and "this list cannot say" must read as "not the default" rather
      // than leaving the version bar with an undefined to interpret.
      { id: 7, name: 'base', status: 'draft', created_at: '2026-01-01T00:00:00Z', is_default: false },
    ]);
  });

  // The default-version flag the Go handler emits (`versions[].is_default`,
  // derived from `applications.meta.default_version_id`) has to survive the
  // mapping, or the version bar is back to only knowing a default it set
  // itself and losing it on reload.
  it("carries the server's default-version flag through", () => {
    const wire = [
      { id: '7', name: 'base', status: 'draft', agent_type: 'classic', created_at: '2026-01-01T00:00:00Z', is_default: false },
      { id: '8', name: 'v1', status: 'draft', agent_type: 'classic', created_at: '2026-02-01T00:00:00Z', is_default: true },
    ] as unknown as readonly ApplicationVersionSummary[];
    expect(toVersionOptions(wire).map((option) => option.is_default)).toEqual([false, true]);
  });

  it('maps an empty list to an empty list', () => {
    expect(toVersionOptions([])).toEqual([]);
  });
});

describe('toVersionWriteBody', () => {
  it('clones the fields the CreateVersion handler actually reads, with the live starters', () => {
    const version = {
      id: '1',
      name: 'base',
      agent_type: 'classic',
      instructions: 'be helpful',
      welcome_message: 'hi',
      llm_settings: { model_name: 'gpt' },
      variables: [{ name: 'k', value: 'v' }],
      meta: { step_limit: 25 },
      tags: [{ name: 'x' }],
    } as unknown as ApplicationVersionDetail;

    expect(toVersionWriteBody(version, ['s1'])).toEqual({
      agent_type: 'classic',
      instructions: 'be helpful',
      welcome_message: 'hi',
      llm_settings: { model_name: 'gpt' },
      conversation_starters: ['s1'],
      variables: [{ name: 'k', value: 'v' }],
      meta: { step_limit: 25 },
    });
  });

  it('omits keys the handler discards and defaults absent text fields to empty strings', () => {
    const version = { id: '1', name: 'base' } as ApplicationVersionDetail;
    const body = toVersionWriteBody(version, []);

    expect(body).toEqual({
      instructions: '',
      welcome_message: '',
      conversation_starters: [],
      variables: [],
      meta: {},
    });
    // `tags` stays omitted — `versionFromBody` reads no `tags` key, so only
    // the PUT writes them. `meta` is NOT in that category; see below.
    expect(body).not.toHaveProperty('tags');
    expect(body).not.toHaveProperty('agent_type');
  });

  /*
   * Save-As-Version used to drop `meta`, on the premise that "the CREATE
   * handler discards it". That premise was false, and the cost was real:
   * `step_limit` silently reset to the handler's default and `internal_tools`
   * vanished — the two keys the native Rust runtime gates admission on, so a
   * cloned agent could stop running while every screen still looked right.
   *
   * Traced end to end before this test was written: `CreateVersion`
   * (`applications/handler.go:811`) calls `versionFromBody`, which reads
   * `vBody["meta"]` (`:504`) and only DEFAULTS `step_limit` when the key is
   * absent; `insertVersion` (`repos/applications.go:517-525`) persists it as
   * the tenth column.
   */
  it('carries `meta` onto the new version, so a clone keeps its step limit and internal tools', () => {
    const version = {
      id: '1',
      name: 'base',
      meta: { step_limit: 40, internal_tools: ['internal_mcp'], category: 'sales' },
    } as unknown as ApplicationVersionDetail;

    const body = toVersionWriteBody(version, [], {
      instructions: 'i',
      welcomeMessage: 'w',
      variables: [],
      stepLimit: 12,
      internalTools: ['internal_mcp', 'internal_web'],
      tags: [],
    } as unknown as EditApplicationVersionFields);

    // The live edits win...
    expect(body.meta?.step_limit).toBe(12);
    expect(body.meta?.internal_tools).toEqual(['internal_mcp', 'internal_web']);
    // ...and a key the editor does not model survives rather than being
    // replaced away, which is the whole point of merging over the stored blob.
    expect(body.meta?.['category']).toBe('sales');
  });

  /*
   * The create path folds the body's TOP-LEVEL `variables` into `meta` only
   * when that list is NON-EMPTY (`applications/handler.go:509-511`). So a
   * `variables` copy forwarded inside `meta` survives a deletion and wins:
   * the new version is inserted with the deleted variable still in
   * `meta.variables` while `application_variables` has no row, and the Rust
   * worker applies the meta copy LAST — substituting a value the user
   * deleted into every turn, while the editor shows no variables at all.
   */
  it('drops the stored `meta.variables` so deleting the last variable is not undone by the clone', () => {
    const version = {
      id: '1',
      name: 'base',
      variables: [{ name: 'api_key', value: 's3cret' }],
      meta: { step_limit: 7, internal_tools: [], variables: [{ name: 'api_key', value: 's3cret' }] },
    } as unknown as ApplicationVersionDetail;

    const body = toVersionWriteBody(version, [], {
      instructions: 'i',
      welcomeMessage: 'w',
      variables: [],
      stepLimit: 7,
      internalTools: [],
      tags: [],
    } as unknown as EditApplicationVersionFields);

    expect(body.variables, 'the authoritative list is the deleted one').toEqual([]);
    expect(
      (body.meta as Record<string, unknown> | undefined)?.['variables'],
      '`meta` must not carry a second, stale copy the server would prefer',
    ).toBeUndefined();
    // The rest of the blob still rides along.
    expect(body.meta?.step_limit).toBe(7);
  });

  it('keeps the stored `meta` verbatim when there are no live edits to fold in', () => {
    const version = {
      id: '1',
      name: 'base',
      meta: { step_limit: 40, internal_tools: ['internal_mcp'] },
    } as unknown as ApplicationVersionDetail;

    expect(toVersionWriteBody(version, []).meta).toEqual({
      step_limit: 40,
      internal_tools: ['internal_mcp'],
    });
  });

  /*
   * #307's argument, applied to the model: while the picker did not exist the
   * edit could not diverge from the server's copy, so reading `llm_settings`
   * off `version` was harmless. Now a Save-As-Version taken after picking a
   * different model would clone the OLD model onto the new version and say
   * nothing about it.
   */
  it('prefers the edited llm_settings over the stored one', () => {
    const version = {
      name: 'base',
      llm_settings: { model_name: 'gpt-4o', model_project_id: 3, max_tokens: 4096 },
    } as unknown as ApplicationVersionDetail;
    const edits = {
      instructions: '',
      welcomeMessage: '',
      variables: [],
      stepLimit: undefined,
      internalTools: [],
      llmSettings: { model_name: 'qwen3.5', model_project_id: 17, max_tokens: -1, temperature: 0.6 },
      tags: [],
      notes: '',
    };

    expect(toVersionWriteBody(version, [], edits).llm_settings).toEqual({
      model_name: 'qwen3.5',
      model_project_id: 17,
      max_tokens: -1,
      temperature: 0.6,
    });
  });

  /*
   * #611-review-3, the write half. The seeded draft round-trips: an edit that
   * never touched the model must put the version's own `reasoning_effort`
   * back on the wire, not a profile without one.
   */
  it('round-trips a reasoning_effort through an edit that never touched the model', () => {
    const version = {
      name: 'base',
      llm_settings: { model_name: 'o3-mini', model_project_id: 17, max_tokens: -1, reasoning_effort: 'high' },
    } as unknown as ApplicationVersionDetail;
    const edits = {
      instructions: 'Edited somewhere else entirely.',
      welcomeMessage: '',
      variables: [],
      stepLimit: undefined,
      internalTools: [],
      llmSettings: toVersionDraft(version, []).llmSettings,
      tags: [],
      notes: '',
    };

    expect(toVersionWriteBody(version, [], edits).llm_settings).toEqual({
      model_name: 'o3-mini',
      model_project_id: 17,
      max_tokens: -1,
      reasoning_effort: 'high',
    });
  });

  /*
   * Verbatim, NOT re-read through `toAgentLlmSettings`. A stored blob naming
   * only a model is a working shape — elitea-main's freeze fills the project
   * id in from the catalogue row it resolves — so a strict read would drop it
   * and move the cloned version onto a different model.
   */
  it('forwards the stored llm_settings unchanged when there is no edit', () => {
    const version = { name: 'base', llm_settings: { model_name: 'gpt' } } as unknown as ApplicationVersionDetail;
    const edits = {
      instructions: '',
      welcomeMessage: '',
      variables: [],
      stepLimit: undefined,
      internalTools: [],
      llmSettings: undefined,
      tags: [],
      notes: '',
    };

    expect(toVersionWriteBody(version, [], edits).llm_settings).toEqual({ model_name: 'gpt' });
  });

  it('omits llm_settings entirely when neither the edit nor the version has one', () => {
    const version = { name: 'base' } as ApplicationVersionDetail;
    expect(Object.hasOwn(toVersionWriteBody(version, []), 'llm_settings')).toBe(false);
  });
});

describe('toVersionSaveBody', () => {
  /*
   * This asserts the CONTRACT, not the mapper: `VersionWriteRequest` is the
   * generated zod schema, and zod object parsing STRIPS keys the schema does
   * not declare. So the assertion below fails outright if `internal_tools`
   * is ever dropped from `VersionMeta` in
   * `services/elitea-main/api/openapi/v2.yaml` — which is exactly the state
   * this test was written to rule out. `zod.record`-style passthrough would
   * not make it pass either: the generated `VersionMeta` is a plain
   * `zod.object`, so the key has to be modelled by name.
   */
  it('survives a round trip through the generated VersionWriteRequest schema', () => {
    const version = {
      name: 'base',
      meta: { step_limit: 40, internal_tools: ['internal_mcp'], category: 'sales' },
    } as unknown as ApplicationVersionDetail;
    const edits = {
      instructions: 'Be helpful.',
      welcomeMessage: '',
      variables: [],
      stepLimit: 12,
      internalTools: ['internal_mcp', 'internal_web'],
      llmSettings: undefined,
      tags: [
        { id: 4, name: 'sales', data: null },
        // A tag the user just typed: `AgentTagEditor` gives it a negative
        // placeholder id, which must NOT reach the wire (#345).
        { id: -1712000000000, name: 'fresh', data: null },
      ],
      notes: 'Ask the design team before changing the tone.',
    };

    const body = toVersionSaveBody(version, [], edits);
    expect(body.meta?.internal_tools).toEqual(['internal_mcp', 'internal_web']);

    const parsed = VersionWriteRequest.parse(body);
    expect(parsed.meta?.internal_tools).toEqual(['internal_mcp', 'internal_web']);
    // The untouched keys ride along through the same merge.
    expect(parsed.meta?.step_limit).toBe(12);
    expect(parsed.meta?.category).toBe('sales');
    // #898 — the Editor Notes field rides in the same `meta` blob.
    expect(parsed.meta?.notes).toBe('Ask the design team before changing the tone.');
    // #345 — `tags` reaches the wire, keyed by name; a stored tag keeps its
    // id and a brand-new one sends none.
    expect(parsed.tags).toEqual([
      { id: 4, name: 'sales' },
      { name: 'fresh' },
    ]);
  });

  /*
   * #345 — the save body must carry `tags` on EVERY save, including the one
   * that empties the list. The Go handler reads the key's presence: an
   * absent key leaves the stored associations alone, so a body that dropped
   * an empty list would make "I removed my last tag" unsaveable, which is
   * exactly the silent-discard shape this issue exists to fix.
   */
  it('sends an empty tags array rather than omitting the key', () => {
    const version = { name: 'base', meta: {} } as unknown as ApplicationVersionDetail;
    const body = toVersionSaveBody(version, [], {
      instructions: '',
      welcomeMessage: '',
      variables: [],
      stepLimit: undefined,
      internalTools: [],
      llmSettings: undefined,
      tags: [],
      notes: '',
    });

    expect(body.tags).toEqual([]);
    expect(Object.hasOwn(body, 'tags')).toBe(true);
  });
});

describe('toVersionSummaries', () => {
  it('maps snake_case fields to the camelCase VersionSummary shape', () => {
    const wire: readonly ApplicationVersionSummary[] = [
      { id: '1', name: 'base', status: 'draft', agent_type: 'classic', created_at: '2026-01-01T00:00:00Z' },
    ];
    expect(toVersionSummaries(wire)).toEqual([
      { id: '1', name: 'base', status: 'draft', agentType: 'classic', createdAt: '2026-01-01T00:00:00Z' },
    ]);
  });

  it('maps an empty list to an empty list', () => {
    expect(toVersionSummaries([])).toEqual([]);
  });
});

describe('applicationDetailDisplayName', () => {
  it('returns the trimmed name when non-blank', () => {
    const detail = { name: 'My Agent' } as ApplicationDetail;
    expect(applicationDetailDisplayName(detail)).toBe('My Agent');
  });

  it('falls back to "Untitled" for a blank name', () => {
    const detail = { name: '   ' } as ApplicationDetail;
    expect(applicationDetailDisplayName(detail)).toBe('Untitled');
  });
});

describe('toFormValues', () => {
  it('seeds name/description from the detail and conversation_starters from the version', () => {
    const detail = { name: 'My Agent', description: 'A helpful agent' } as ApplicationDetail;
    const version = { conversation_starters: ['Hi', null, undefined, 'Bye'] } as unknown as ApplicationVersionDetail;
    expect(toFormValues(detail, version)).toEqual({
      name: 'My Agent',
      description: 'A helpful agent',
      version_details: { conversation_starters: ['Hi', 'Bye'] },
    });
  });

  it('defaults conversation_starters to [] when there is no version yet', () => {
    const detail = { name: 'My Agent', description: 'A helpful agent' } as ApplicationDetail;
    expect(toFormValues(detail, undefined)).toEqual({
      name: 'My Agent',
      description: 'A helpful agent',
      version_details: { conversation_starters: [] },
    });
  });
});

describe('toVersionDraft', () => {
  it('carries over name/instructions/variables/tools and the new conversation starters', () => {
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
      agentType: undefined,
      instructions: 'Be helpful.',
      conversationStarters: ['Hi there'],
      variables: [{ name: 'x', value: '1' }],
      meta: { step_limit: 40, internal_tools: ['internal_mcp', 'other'] },
      tags: ['sales'],
      tools: [{ id: 't1' }],
      pipelineSettings: undefined,
    });
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

  it('sets agentType to "pipeline" only when the version agent_type is exactly "pipeline"', () => {
    const version = { name: 'base', agent_type: 'pipeline' } as ApplicationVersionDetail;
    expect(toVersionDraft(version, []).agentType).toBe('pipeline');
  });

  it('reads the stored llm_settings back, with model_project_id as a number', () => {
    const version = {
      name: 'base',
      llm_settings: { model_name: 'qwen3.5', model_project_id: '17', max_tokens: -1, temperature: 0.6 },
    } as unknown as ApplicationVersionDetail;

    expect(toVersionDraft(version, []).llmSettings).toEqual({
      model_name: 'qwen3.5',
      model_project_id: 17,
      max_tokens: -1,
      temperature: 0.6,
    });
  });

  /*
   * #611-review-3, the seed half. An edit page starts from this draft, so a
   * key dropped here is dropped from the version on the NEXT save of any
   * field — the author edits the instructions, saves, and the reasoning
   * budget they picked is gone with no error anywhere.
   */
  it('reads a stored reasoning_effort back into the draft', () => {
    const version = {
      name: 'base',
      llm_settings: { model_name: 'o3-mini', model_project_id: 17, max_tokens: -1, reasoning_effort: 'high' },
    } as unknown as ApplicationVersionDetail;

    expect(toVersionDraft(version, []).llmSettings).toEqual({
      model_name: 'o3-mini',
      model_project_id: 17,
      max_tokens: -1,
      reasoning_effort: 'high',
    });
  });

  // `{}` is what every version written before the picker existed stores, and
  // the omitted key is what keeps those agents on the catalogue-default
  // fallback they answer turns with today.
  it('leaves llmSettings undefined for a version that names no model', () => {
    const version = { name: 'base', llm_settings: {} } as unknown as ApplicationVersionDetail;
    expect(toVersionDraft(version, []).llmSettings).toBeUndefined();
  });
});
