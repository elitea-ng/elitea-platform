// The elitea.agent/v1 and elitea.team/v1 file format (issue 3137): schema,
// every front-matter key, avatars, follow-ups and the server's refusal.
import { describe, expect, it, vi } from 'vitest';

import JSZip from 'jszip';

import { EliteaApiError } from '@/shared/api/generated/mutator';

import { attachImportAvatars } from './importAvatars';
import { ImportDocumentError, isImportableFileName, readImportFile } from './importDocument';
import { importFollowUps, importRefusalOf } from './importFollowUps';

type Rec = Record<string, unknown>;

const RICH = `---
schema: elitea.agent/v1
name: Incident Triage
description: Sets severity
agent_type: react
reasoning_effort: medium
step_limit: 40
internal_tools: [planner]
tags: [ops, incidents]
welcome_message: Hello
conversation_starters: [What broke?]
soul: { purpose: Triage incidents }
avatar: default:owl
memory: { enabled: true, write: review }
context: { history: 20 }
proactivity: { mode: heartbeat, interval: 30m }
tool_envelope: { tool_limits: { search: 5 } }
capabilities: { propose: true }
run_caps: { max_tool_calls: 10 }
output_contract: { format: json }
citations: { required: true }
evaluation: { datasets: [{ name: smoke, cases: [] }] }
memory_seeds: [{ text: Escalate sev1 to on-call }, { text: Use UTC }]
blueprint: { brief: An incident triager, surface: cloud }
---
You triage incidents.
`;

function mdFile(text: string, name = 'agent.agent.md'): File {
  return new File([text], name, { type: 'text/markdown' });
}

async function zipFile(files: Record<string, string | Uint8Array>, name: string): Promise<File> {
  const zip = new JSZip();
  for (const [path, content] of Object.entries(files)) zip.file(path, content);
  return new File([await zip.generateAsync({ type: 'blob' })], name, { type: 'application/zip' });
}

function firstEntry(result: { readonly document: { readonly applications?: readonly unknown[] | undefined } }): Rec {
  return (result.document.applications?.[0] ?? {}) as Rec;
}

function firstVersion(entry: Rec): Rec {
  return (entry['versions'] as Rec[] | undefined)?.[0] ?? {};
}

describe('elitea.agent/v1 markdown', () => {
  it('maps every key of the format and ignores blueprint', async () => {
    const result = await readImportFile(mdFile(RICH));
    const entry = firstEntry(result);
    const version = firstVersion(entry);
    expect(entry['name']).toBe('Incident Triage');
    expect(entry['description']).toBe('Sets severity');
    expect(entry['evaluation']).toEqual({ datasets: [{ name: 'smoke', cases: [] }] });
    expect(entry['memory_seeds']).toHaveLength(2);
    expect(version['agent_type']).toBe('react');
    expect(version['llm_settings']).toEqual({ reasoning_effort: 'medium' });
    expect(version['welcome_message']).toBe('Hello');
    expect(version['conversation_starters']).toEqual(['What broke?']);
    expect(version['soul']).toEqual({ purpose: 'Triage incidents' });
    expect(version['instructions']).toContain('You triage incidents.');
    expect(version['avatar_pack']).toEqual({ schema: 'elitea.avatar_pack/v1', slots: { still: { url: 'default:owl' } } });
    expect(version['meta']).toEqual({
      step_limit: 40,
      internal_tools: ['planner'],
      memory: { enabled: true, write: 'review' },
      context: { history: 20 },
      proactivity: { mode: 'heartbeat', interval: '30m' },
      tool_envelope: { tool_limits: { search: 5 } },
      capabilities: { propose: true },
      run_caps: { max_tool_calls: 10 },
      output_contract: { format: 'json' },
      citations: { required: true },
    });
    expect(JSON.stringify(result.document)).not.toContain('An incident triager');
    expect(result.skipped).toEqual([]);
  });

  it('refuses a schema this version cannot read, naming it', async () => {
    const text = RICH.replace('schema: elitea.agent/v1', 'schema: elitea.agent/v2');
    await expect(readImportFile(mdFile(text))).rejects.toThrow(/elitea\.agent\/v2/);
    await expect(readImportFile(mdFile(text))).rejects.toBeInstanceOf(ImportDocumentError);
  });

  it('still reads a file without schema', async () => {
    const result = await readImportFile(mdFile('---\nname: Plain\n---\nBody\n'));
    expect(firstEntry(result)['name']).toBe('Plain');
  });

  it('lists an avatar path map in skipped for a standalone .md: its files travel only in a zip', async () => {
    const result = await readImportFile(mdFile('---\nname: Pic\navatar:\n  still: avatars/still.webp\n---\nBody\n'));
    expect(result.skipped.join(' ')).toMatch(/Pic: avatar/);
    expect(firstVersion(firstEntry(result))['avatar_pack']).toBeUndefined();
    expect(result.avatars ?? []).toEqual([]);
  });

  it('accepts the .agent.zip extension', () => {
    expect(isImportableFileName('scout.agent.zip')).toBe(true);
    expect(isImportableFileName('scout.agent.md')).toBe(true);
  });
});

describe('avatars from a zip', () => {
  const md = '---\nschema: elitea.agent/v1\nname: Pic\navatar:\n  still: avatars/still.webp\n  thinking: avatars/thinking.webm\n---\nBody\n';

  it('reads the slot files of an .agent.zip and uploads them into the version pack on confirm', async () => {
    const result = await readImportFile(
      await zipFile({ 'pic.agent.md': md, 'avatars/still.webp': new Uint8Array([1]), 'avatars/thinking.webm': new Uint8Array([2]) }, 'pic.agent.zip'),
    );
    expect(result.avatars?.map((avatar) => [avatar.slot, avatar.path])).toEqual([
      ['still', 'avatars/still.webp'],
      ['thinking', 'avatars/thinking.webm'],
    ]);
    const upload = vi.fn((file: File) => Promise.resolve({ url: `/media/${file.name}`, media_type: file.type }));
    const document = await attachImportAvatars(result.document, result.avatars ?? [], upload);
    expect(upload).toHaveBeenCalledTimes(2);
    expect(upload.mock.calls[0]?.[0].type).toBe('image/webp');
    expect(firstVersion(firstEntry({ document }))['avatar_pack']).toEqual({
      schema: 'elitea.avatar_pack/v1',
      slots: {
        still: { url: '/media/still.webp', media_type: 'image/webp' },
        thinking: { url: '/media/thinking.webm', media_type: 'video/webm' },
      },
    });
  });

  it('names a slot file the zip does not hold', async () => {
    await expect(
      readImportFile(await zipFile({ 'pic.agent.md': md, 'avatars/still.webp': new Uint8Array([1]) }, 'pic.agent.zip')),
    ).rejects.toThrow(/thinking\.webm/);
  });

  it('refuses an avatar file over 2 MiB', async () => {
    const big = new Uint8Array(2 * 1024 * 1024 + 1);
    await expect(
      readImportFile(
        await zipFile({ 'pic.agent.md': md, 'avatars/still.webp': big, 'avatars/thinking.webm': new Uint8Array([2]) }, 'pic.agent.zip'),
      ),
    ).rejects.toBeInstanceOf(ImportDocumentError);
  });

  it('resolves team member avatars against the team.yaml folder', async () => {
    const team = 'schema: elitea.team/v1\nkind: team\nname: Desk\nmembers: [members/pic.agent.md]\n';
    const result = await readImportFile(
      await zipFile(
        {
          'team.yaml': team,
          'members/pic.agent.md': md,
          'avatars/still.webp': new Uint8Array([1]),
          'avatars/thinking.webm': new Uint8Array([2]),
        },
        'desk.team.zip',
      ),
    );
    expect(result.team?.name).toBe('Desk');
    expect(result.avatars).toHaveLength(2);
  });

  it('refuses a team.yaml of an unknown schema', async () => {
    const team = 'schema: elitea.team/v9\nname: Desk\nmembers: [a.agent.md]\n';
    await expect(readImportFile(await zipFile({ 'team.yaml': team, 'a.agent.md': '---\nname: A\n---\n' }, 'd.team.zip'))).rejects.toThrow(
      /elitea\.team\/v9/,
    );
  });
});

describe('import follow-ups', () => {
  it('counts memory seeds, names heartbeat agents and the toolkits the server says need a connection', async () => {
    const { document } = await readImportFile(mdFile(RICH));
    const followUps = importFollowUps(document, {
      toolkits: [
        { name: 'Jira', type: 'jira', needs_connection: true },
        { name: 'Calc', type: 'calc', needs_connection: false },
      ],
    });
    expect(followUps).toEqual({ toolkits: [{ name: 'Jira', type: 'jira' }], memorySeeds: 2, heartbeatAgents: ['Incident Triage'], models: [] });
  });

  it('names no toolkit when the server listed them and marked none', () => {
    const followUps = importFollowUps({ applications: [{ name: 'A' }], toolkits: [{ name: 'Calc', type: 'calc' }] }, { toolkits: [{ name: 'Calc', type: 'calc' }] });
    expect(followUps.toolkits).toEqual([]);
  });

  it('names every toolkit of the file when the server answered no list', () => {
    const followUps = importFollowUps({ applications: [{ name: 'A' }], toolkits: [{ name: 'GitHub', type: 'github' }] });
    expect(followUps.toolkits).toEqual([{ name: 'GitHub', type: 'github' }]);
    expect(followUps.heartbeatAgents).toEqual([]);
  });
});

describe('importRefusalOf', () => {
  it('reads the import_invalid refusal and the field it names', () => {
    const error = new EliteaApiError({
      kind: 'http',
      status: 422,
      url: 'https://example.test/x',
      body: { error: 'run_caps.max_tool_calls is above the tier cap', code: 'import_invalid', field: 'applications[0].versions[0].meta.run_caps.max_tool_calls' },
    });
    expect(importRefusalOf(error)).toEqual({
      message: 'run_caps.max_tool_calls is above the tier cap',
      field: 'applications[0].versions[0].meta.run_caps.max_tool_calls',
    });
  });

  it('is undefined for any other failure', () => {
    expect(importRefusalOf(new Error('x'))).toBeUndefined();
    expect(importRefusalOf(new EliteaApiError({ kind: 'http', status: 400, url: 'u', body: { error: 'bad' } }))).toBeUndefined();
  });
});
