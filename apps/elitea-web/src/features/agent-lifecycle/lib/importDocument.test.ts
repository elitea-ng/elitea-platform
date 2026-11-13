import { describe, expect, it } from 'vitest';

import JSZip from 'jszip';

import {
  ImportDocumentError,
  parseImportDocument,
  parseMarkdownAgent,
  readImportFile,
  summarizeImportDocument,
} from './importDocument';

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

describe('parseMarkdownAgent (#1996)', () => {
  it('turns frontmatter and body into an import entry', () => {
    const entry = parseMarkdownAgent(
      '---\nname: Doc Bot\ndescription: answers\nmodel: gpt-5\ntemperature: 0\nagent_type: agent\nstep_limit: 5\n---\n\nBe helpful.\n',
    ) as { name: string; entity: string; versions: { instructions: string; llm_settings: Record<string, unknown>; meta: Record<string, unknown> }[] };
    expect(entry.name).toBe('Doc Bot');
    expect(entry.entity).toBe('agents');
    expect(entry.versions[0]?.instructions).toBe('Be helpful.');
    expect(entry.versions[0]?.llm_settings).toEqual({ model_name: 'gpt-5', temperature: 0 });
    expect(entry.versions[0]?.meta).toEqual({ step_limit: 5 });
  });

  it('refuses a file with no frontmatter or no name', () => {
    expect(() => parseMarkdownAgent('hello')).toThrow(/frontmatter/);
    expect(() => parseMarkdownAgent('---\ndescription: x\n---\nbody')).toThrow(/no name/);
  });
});

type Entry = {
  import_uuid: string;
  versions: {
    import_version_uuid: string;
    tools: { import_uuid: string }[];
    skills: { import_uuid: string; version_name: string }[];
    tags: unknown[];
    soul?: unknown;
  }[];
};

describe('markdown import carries what the file holds (#2032)', () => {
  const full =
    '---\nname: Doc Bot\ntags:\n  - alpha\n  - {name: beta, data: {c: 1}}\ntoolkits:\n  - toolkit: GitHub\n    type: github\n    settings: {repo: a/b}\n    tools: [get_issue]\n  - toolkit: Helper\n    type: application\nskills:\n  - {name: Reviewer, description: d, version: v2, instructions: Review it}\nsoul: {voice: dry}\n---\n\nBody';

  it('keeps tags, toolkits, skills and soul instead of dropping them', async () => {
    const { document, skipped } = await readImportFile(new File([full], 'doc.agent.md'));
    const entry = document.applications?.[0] as Entry;
    const version = entry.versions[0];
    expect(version?.tags).toEqual([{ name: 'alpha' }, { name: 'beta', data: { c: 1 } }]);
    expect(version?.soul).toEqual({ voice: 'dry' });
    const toolkit = document.toolkits?.[0] as {
      import_uuid: string;
      entity: string;
      settings: Record<string, unknown>;
    };
    expect(toolkit.entity).toBe('toolkits');
    expect(toolkit.settings).toEqual({ repo: 'a/b', selected_tools: ['get_issue'] });
    expect(version?.tools).toEqual([{ import_uuid: toolkit.import_uuid }]);
    const skill = document.skills?.[0] as { import_uuid: string; versions: { name: string; instructions: string }[] };
    expect(skill.versions).toEqual([{ name: 'v2', instructions: 'Review it' }]);
    expect(version?.skills).toEqual([{ import_uuid: skill.import_uuid, version_name: 'v2' }]);
    // The nested agent cannot be rebuilt from the file: shown, not dropped silently.
    expect(skipped).toEqual(['Doc Bot: Helper (nested agent)']);
  });

  it('gives non-Latin and punctuation-different names distinct keys and does not claim an export', () => {
    const entries = ['日本語', '中文', 'A-B', 'A B'].map(
      (n) => parseMarkdownAgent(`---\nname: ${n}\n---\nx`) as unknown as Entry,
    );
    expect(new Set(entries.map((e) => e.import_uuid)).size).toBe(4);
    expect(entries[0]).not.toHaveProperty('original_exported');
  });
});

describe('import size limits (#2032)', () => {
  const agent = (n: string): string => `---\nname: ${n}\n---\nbody`;

  it('refuses a single .md over 1 MiB', async () => {
    const file = new File([agent('x') + 'a'.repeat(1024 * 1024)], 'big.md');
    await expect(readImportFile(file)).rejects.toThrow(/import limit/);
  });

  it('refuses a zip with more than 50 agent files', async () => {
    const zip = new JSZip();
    for (let i = 0; i < 51; i += 1) zip.file(`a${String(i)}.md`, agent(`n${String(i)}`));
    const file = new File([await zip.generateAsync({ type: 'arraybuffer' })], 'many.zip');
    await expect(readImportFile(file)).rejects.toThrow(/more than 50/);
  });

  it('refuses an entry over 1 MiB before inflating it', async () => {
    const zip = new JSZip();
    zip.file('huge.md', agent('h') + 'a'.repeat(2 * 1024 * 1024));
    const file = new File([await zip.generateAsync({ type: 'arraybuffer', compression: 'DEFLATE' })], 'huge.zip');
    await expect(readImportFile(file)).rejects.toThrow(/huge\.md is larger than 1 MiB/);
  });

  it('refuses a zip whose entries together pass 10 MiB', async () => {
    const zip = new JSZip();
    const chunk = 'a'.repeat(900 * 1024);
    for (let i = 0; i < 12; i += 1) zip.file(`p${String(i)}.md`, agent(`p${String(i)}`) + chunk);
    const file = new File([await zip.generateAsync({ type: 'arraybuffer', compression: 'DEFLATE' })], 'total.zip');
    await expect(readImportFile(file)).rejects.toThrow(/total\.zip is larger than 10 MiB/);
  });

  it('reads a small zip into one document with every agent', async () => {
    const zip = new JSZip();
    zip.file('a.md', agent('A'));
    zip.file('b.md', agent('B'));
    const file = new File([await zip.generateAsync({ type: 'arraybuffer' })], 'ok.zip');
    const { document } = await readImportFile(file);
    expect(document.applications).toHaveLength(2);
  });
});
