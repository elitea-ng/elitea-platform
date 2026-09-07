/**
 * The three encoding layers between a terminal poll and a tool's own answer.
 *
 * Delete these tests and every layer can be dropped silently. Reading only
 * layer 1 hands a screen the whole SPI envelope as its data; forgetting layer 3
 * renders a wall of escaped JSON where rows belong; and an unpeeled refusal
 * puts the marshalled result list in the error banner, which reports the
 * failure and hides the reason. All of them look like a provider defect on
 * screen, and none of them is one.
 */
import { describe, expect, it } from 'vitest';

import { toolArtifacts, toolErrorText, toolResultDocument, toolResultText } from './toolResult';

/** One SPI result list, as `spi.Completed` marshals it. */
function envelope(...objects: readonly Record<string, unknown>[]): string {
  return JSON.stringify(objects);
}

const MESSAGE = { object_type: 'message', result_target: 'response', result_encoding: 'plain' };

describe('toolResultText', () => {
  it('peels the message out of the marshalled result list', () => {
    expect(toolResultText(envelope({ ...MESSAGE, data: '{"node_count":6}' }))).toBe(
      '{"node_count":6}',
    );
  });

  it('reads a poll with no result as no answer rather than throwing', () => {
    expect(toolResultText(undefined)).toBe('');
    expect(toolResultText('   ')).toBe('');
  });

  it('returns a plain answer unchanged, because that text IS the answer', () => {
    // A provider that answers the message directly is answering. Refusing it
    // would blank a screen that a working engine had just filled.
    expect(toolResultText('Types are already normalised.')).toBe('Types are already normalised.');
  });

  it('returns the raw text when the list cannot be parsed', () => {
    // A truncated body is still evidence. Throwing here would replace the
    // provider's own partial answer with a stack trace.
    expect(toolResultText('[{"object_type":"message"')).toBe('[{"object_type":"message"');
  });

  it('reads a parsed list with no message as an empty answer', () => {
    expect(toolResultText('[]')).toBe('');
    expect(toolResultText('["not an object"]')).toBe('');
  });

  it('reads a message whose data is not a string as an empty answer', () => {
    expect(toolResultText(envelope({ ...MESSAGE, data: { node_count: 6 } }))).toBe('');
  });

  it('answers the empty string for a body of artifacts and no message', () => {
    // The tool answered, and what it answered is in the bucket. Returning the
    // raw envelope here is what puts escaped JSON on screen.
    expect(
      toolResultText(envelope({ name: 'graph.json', result_target: 'artifact', data: 'a graph' })),
    ).toBe('');
  });

  it('skips entries that are not objects while looking for the message', () => {
    expect(toolResultText('[null, 4, {"object_type":"message","data":"ok"}]')).toBe('ok');
  });
});

describe('toolArtifacts', () => {
  it('names what an ingestion wrote, without the payload', () => {
    // The checkpoint is named in the terminal body and nowhere else a screen
    // can reach, so a panel that ignores the artifacts cannot report it at all.
    const artifacts = toolArtifacts(
      envelope(
        { ...MESSAGE, data: 'Ingested 4 entities.' },
        {
          name: 'graph.json',
          object_type: 'knowledge_graph',
          result_target: 'artifact',
          result_bucket: 'graphs',
          data: 'a megabyte of graph',
        },
        {
          name: '.ingestion-checkpoint-9010.json',
          object_type: 'checkpoint',
          result_target: 'artifact',
          data: 'a checkpoint',
        },
      ),
    );
    expect(artifacts).toEqual([
      { name: 'graph.json', objectType: 'knowledge_graph' },
      { name: '.ingestion-checkpoint-9010.json', objectType: 'checkpoint' },
    ]);
  });

  it('drops an artifact with no name, which nothing can be said about', () => {
    expect(toolArtifacts(envelope({ result_target: 'artifact', object_type: 'graph' }))).toEqual([]);
  });

  it('reports an empty object_type rather than the string "undefined"', () => {
    expect(toolArtifacts(envelope({ name: 'x.json', result_target: 'artifact' }))).toEqual([
      { name: 'x.json', objectType: '' },
    ]);
  });

  it('answers nothing for a body that is not the marshalled list', () => {
    expect(toolArtifacts(undefined)).toEqual([]);
    expect(toolArtifacts('Ingestion finished.')).toEqual([]);
    expect(toolArtifacts('[not json')).toEqual([]);
    expect(toolArtifacts('[1, null]')).toEqual([]);
  });
});

describe('toolResultDocument', () => {
  it('parses the JSON document an output_format:json read answered', () => {
    expect(
      toolResultDocument(envelope({ ...MESSAGE, data: '{"node_count":6,"edge_count":5}' })),
    ).toEqual({ node_count: 6, edge_count: 5 });
  });

  it('answers undefined for markdown, which is not a document', () => {
    // A read that forgot `output_format` gets a formatted report. The caller
    // renders no rows for it; casting it would render `undefined` as data.
    expect(
      toolResultDocument(envelope({ ...MESSAGE, data: '# Report\n\n6 entities.' })),
    ).toBeUndefined();
  });

  it('answers undefined for a document that is not an object', () => {
    expect(toolResultDocument(envelope({ ...MESSAGE, data: '[1,2,3]' }))).toBeUndefined();
    expect(toolResultDocument(envelope({ ...MESSAGE, data: '   ' }))).toBeUndefined();
    expect(toolResultDocument(undefined)).toBeUndefined();
  });
});

describe('toolErrorText', () => {
  it('peels a refusal, because a failure carries the same envelope a success does', () => {
    expect(
      toolErrorText(envelope({ ...MESSAGE, data: "Entity 'code:nope' is not in this graph." })),
    ).toBe("Entity 'code:nope' is not in this graph.");
  });

  it('keeps a facade refusal, which has no envelope around it', () => {
    // A 403 for a source this toolkit does not own is a plain sentence.
    // Falling back to the empty string would leave a banner with nothing in it.
    expect(toolErrorText('This toolkit does not own source 9010.')).toBe(
      'This toolkit does not own source 9010.',
    );
  });

  it('falls back to the raw text when the envelope peels to nothing', () => {
    const artifactsOnly = envelope({ name: 'graph.json', result_target: 'artifact' });
    expect(toolErrorText(artifactsOnly)).toBe(artifactsOnly);
  });

  it('answers the empty string for a refusal with no message at all', () => {
    expect(toolErrorText(undefined)).toBe('');
  });
});
