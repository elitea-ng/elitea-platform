import { describe, expect, it } from 'vitest';

import { ImportDocumentError, parseImportDocument, summarizeImportDocument } from './importDocument';

const exported = JSON.stringify({
  ok: true,
  applications: [{ name: 'Audit Agent', description: 'checks things', versions: [] }],
  toolkits: [{ name: 'GitHub' }],
  skills: [{ name: 'Reviewer' }],
});

describe('parseImportDocument', () => {
  it('accepts the document this app exports', () => {
    const document = parseImportDocument(exported);
    expect(document.applications).toHaveLength(1);
    expect(document.toolkits).toHaveLength(1);
    expect(document.skills).toHaveLength(1);
  });

  it('refuses a file that is not JSON', () => {
    expect(() => parseImportDocument('# a markdown skill')).toThrow(ImportDocumentError);
  });

  // A skill export is Markdown, not JSON — but a JSON file from another tool
  // parses fine and carries no `applications`. Posting it reaches the server's
  // own 400 about a key the user never saw.
  it('refuses valid JSON that holds no agents', () => {
    expect(() => parseImportDocument('{"ok": true, "toolkits": []}')).toThrow(/holds no agents/);
  });

  it('refuses an empty applications array rather than importing nothing', () => {
    expect(() => parseImportDocument('{"applications": []}')).toThrow(/holds no agents/);
  });

  it('refuses a JSON scalar', () => {
    expect(() => parseImportDocument('42')).toThrow(ImportDocumentError);
    expect(() => parseImportDocument('null')).toThrow(ImportDocumentError);
  });
});

describe('summarizeImportDocument', () => {
  it('names every agent and counts the attachments', () => {
    const summary = summarizeImportDocument(parseImportDocument(exported));
    expect(summary.names).toEqual(['Audit Agent']);
    expect(summary.toolkitCount).toBe(1);
    expect(summary.skillCount).toBe(1);
  });

  it('gives a positional name to an entry that has none', () => {
    const summary = summarizeImportDocument({ applications: [{}, { name: '' }] });
    expect(summary.names).toEqual(['#1', '#2']);
  });

  it('counts an absent toolkits or skills key as zero, not undefined', () => {
    const summary = summarizeImportDocument({ applications: [{ name: 'A' }] });
    expect(summary.toolkitCount).toBe(0);
    expect(summary.skillCount).toBe(0);
  });
});
