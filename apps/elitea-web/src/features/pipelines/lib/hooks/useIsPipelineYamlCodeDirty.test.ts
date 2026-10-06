import { describe, expect, it } from 'vitest';

import { computeIsPipelineYamlCodeDirty, isChatPath, isPipelineDetailPath } from './useIsPipelineYamlCodeDirty';

describe('isPipelineDetailPath', () => {
  it('matches /pipelines/create', () => {
    expect(isPipelineDetailPath('/pipelines/create')).toBe(true);
  });

  it('matches /pipelines/:tab/:agentId', () => {
    expect(isPipelineDetailPath('/pipelines/latest/123')).toBe(true);
  });

  it('matches /pipelines/:tab/:agentId/:version', () => {
    expect(isPipelineDetailPath('/pipelines/latest/123/v2')).toBe(true);
  });

  it('does not match the bare pipelines list route', () => {
    expect(isPipelineDetailPath('/pipelines')).toBe(false);
    expect(isPipelineDetailPath('/pipelines/latest')).toBe(false);
  });

  it('does not match an unrelated route', () => {
    expect(isPipelineDetailPath('/agents/latest/123')).toBe(false);
  });
});

describe('isChatPath', () => {
  it('matches any /chat prefix', () => {
    expect(isChatPath('/chat')).toBe(true);
    expect(isChatPath('/chat/abc-123')).toBe(true);
  });

  it('does not match a non-chat path', () => {
    expect(isChatPath('/pipelines/latest')).toBe(false);
  });
});

describe('computeIsPipelineYamlCodeDirty', () => {
  /*
   * The regression this file exists to hold down (#613). `EditorPanel`'s
   * `setYamlJsonObject` re-dumps the parsed document and overwrites
   * `yamlCode` alone, leaving `initYamlCode` on the raw stored text — so the
   * live text is routinely a THIRD spelling of an UNCHANGED document. Calling
   * that an edit armed the unsaved-changes guard on a pipeline nobody had
   * touched: the test-chat pane stayed closed, and the "Chat with pipeline"
   * button's own navigation opened a "You have unsaved changes" dialog
   * instead of going anywhere. Reproduced at ~25% in
   * `e2e/streaming/chat.pipeline.spec.ts`.
   */
  it('is false when the live text is the same document in a different YAML style', () => {
    const stored = 'state:\n  answer: str\nnodes:\n  - id: LLM_1\n    input_mapping:\n      task: {type: variable, value: input}\n';
    const reDumped = 'state:\n  answer: str\nnodes:\n  - id: LLM_1\n    input_mapping:\n      task:\n        type: variable\n        value: input\n';
    expect(computeIsPipelineYamlCodeDirty('/pipelines/latest/1', reDumped, stored)).toBe(false);
  });

  it('is false when only the key ORDER differs — the editor rebuilds objects by spreading', () => {
    expect(computeIsPipelineYamlCodeDirty('/pipelines/latest/1', 'b: 2\na: 1\n', 'a: 1\nb: 2\n')).toBe(false);
  });

  /*
   * The half the textual check was protecting, kept. A legacy-node migration
   * REWRITES the document rather than reformatting it, so it must still count
   * as dirty — otherwise the migration's own rewrite becomes unsaveable.
   */
  it('is true when the document actually changed, not just its formatting', () => {
    expect(computeIsPipelineYamlCodeDirty('/pipelines/latest/1', 'a: 1\nb: 3\n', 'a: 1\nb: 2\n')).toBe(true);
  });

  it('is true when array ORDER changed — `nodes:` is a sequence and its order is meaningful', () => {
    expect(computeIsPipelineYamlCodeDirty('/pipelines/latest/1', 'nodes: [b, a]\n', 'nodes: [a, b]\n')).toBe(true);
  });

  it('is true while the live text does not parse — an edit in progress is not a match', () => {
    expect(computeIsPipelineYamlCodeDirty('/pipelines/latest/1', 'a: [unclosed\n', 'a: 1\n')).toBe(true);
  });

  it('is false outside pipeline-editing screens even when the code differs', () => {
    expect(computeIsPipelineYamlCodeDirty('/agents/latest/1', 'a: 2', 'a: 1')).toBe(false);
  });

  it('is false on a pipeline detail page when the code is identical to the saved snapshot', () => {
    expect(computeIsPipelineYamlCodeDirty('/pipelines/latest/1', 'a: 1', 'a: 1')).toBe(false);
  });

  it('is true on a pipeline detail page when the code differs from the saved snapshot', () => {
    expect(computeIsPipelineYamlCodeDirty('/pipelines/latest/1', 'a: 2', 'a: 1')).toBe(true);
  });

  it('is true on a chat page when the code differs from the saved snapshot', () => {
    expect(computeIsPipelineYamlCodeDirty('/chat/conv-1', 'a: 2', 'a: 1')).toBe(true);
  });

  it('is false when the current code equals the canonical re-dump of the saved snapshot, even though its raw text differs', () => {
    // The saved snapshot's raw text has keys in a different order than
    // `dumpYaml`'s canonical (sorted) form would produce — the current code
    // below IS that canonical form, so it counts as clean even though it is
    // not byte-identical to `initYamlCode`.
    const initYamlCode = 'b: 2\na: 1\n';
    const canonicalRedump = 'a: 1\nb: 2\n';
    expect(computeIsPipelineYamlCodeDirty('/pipelines/latest/1', canonicalRedump, initYamlCode)).toBe(false);
  });

  it('treats unparsable saved-snapshot YAML as producing an empty re-dump, so any non-empty current code is dirty', () => {
    expect(computeIsPipelineYamlCodeDirty('/pipelines/latest/1', 'a: 1', '::: not yaml [')).toBe(true);
  });
});

describe('meaningful pipeline state order', () => {
  it('reports an ordinary state declaration reorder as dirty', () => {
    expect(
      computeIsPipelineYamlCodeDirty('/pipelines/latest/1', 'state: {a: str, z: list}', 'state: {z: list, a: str}'),
    ).toBe(true);
  });

  it('reports quoted integer-like declaration order changes as dirty', () => {
    expect(
      computeIsPipelineYamlCodeDirty(
        '/pipelines/latest/1',
        'state: {"2": dict, "10": list}',
        'state: {"10": list, "2": dict}',
      ),
    ).toBe(true);
  });

  it('keeps the same declaration sequence clean across formatting changes', () => {
    expect(
      computeIsPipelineYamlCodeDirty(
        '/pipelines/latest/1',
        'state:\n  "10": list\n  "2": dict\n',
        'state: {"10": list, "2": dict}',
      ),
    ).toBe(false);
  });

  it('detects unknown field and explicit-null changes', () => {
    expect(
      computeIsPipelineYamlCodeDirty('/chat/a', 'state: {a: {type: dict, value: null}}', 'state: {a: {type: dict}}'),
    ).toBe(true);
    expect(
      computeIsPipelineYamlCodeDirty('/chat/a', 'state: {a: {type: dict, future: true}}', 'state: {a: {type: dict}}'),
    ).toBe(true);
  });
});
