/**
 * The empty project's offer by folder kind: a folder of documents (or a mixed one) offers the Local documents agent
 * first, a git or code folder the Local coding agent; both are always offered. With agents in the project, the
 * session picks the folder's default Local agent when nothing is remembered.
 */
import { screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { NoAgents } from './AgentPickers';
import { defaultAgentId } from './useAgentSelection';
import { localAgentBody, type LocalCodingAgent } from './useLocalCodingAgent';

function offer(): LocalCodingAgent {
  return { state: 'ready', creating: false, error: undefined, create: vi.fn() };
}

function buttons(): string[] {
  const box = screen.getByTestId('workspace-no-agents');
  return within(box)
    .getAllByRole('button')
    .map((button) => button.textContent ?? '');
}

describe('NoAgents by folder kind', () => {
  it.each(['documents', 'mixed'] as const)('offers the Local documents agent first in a %s folder, and creates it on a click', async (kind) => {
    const agent = offer();
    renderWithTheme(<NoAgents codingAgent={agent} folderKind={kind} onCreate={vi.fn()} otherProjects={[]} onMoveTo={vi.fn()} busy={false} />);
    expect(screen.getByText(/^Create a Local documents agent\?/)).toBeInTheDocument();
    expect(buttons().slice(0, 2)).toEqual(['Create a Local documents agent', 'Create a Local coding agent']);
    await userEvent.click(screen.getByRole('button', { name: 'Create a Local documents agent' }));
    expect(agent.create).toHaveBeenCalledWith('documents');
  });

  it.each(['git', 'code', undefined] as const)('offers the Local coding agent first in a %s folder; the documents agent stays selectable', async (kind) => {
    const agent = offer();
    renderWithTheme(<NoAgents codingAgent={agent} folderKind={kind} onCreate={vi.fn()} otherProjects={[]} onMoveTo={vi.fn()} busy={false} />);
    expect(screen.getByText(/^Create a Local coding agent\?/)).toBeInTheDocument();
    expect(buttons().slice(0, 2)).toEqual(['Create a Local coding agent', 'Create a Local documents agent']);
    await userEvent.click(screen.getByRole('button', { name: 'Create a Local documents agent' }));
    expect(agent.create).toHaveBeenCalledWith('documents');
  });
});

describe('defaultAgentId', () => {
  const agents = [
    { id: '1', name: 'Reviewer' },
    { id: '2', name: 'Local coding agent' },
    { id: '3', name: 'Local documents agent' },
  ];
  it('picks the folder kind default Local agent, else the first agent', () => {
    expect(defaultAgentId(agents, 'documents')).toBe('3');
    expect(defaultAgentId(agents, 'mixed')).toBe('3');
    expect(defaultAgentId(agents, 'code')).toBe('2');
    expect(defaultAgentId(agents, 'git')).toBe('2');
    expect(defaultAgentId([agents[0]!], 'documents')).toBe('1');
    expect(defaultAgentId([], 'documents')).toBe('');
  });
});

describe('the Local documents agent', () => {
  it('is created with the shipped documents instructions: read_document, edits to a new Office file, undo, no tests', () => {
    const body = localAgentBody('documents', { name: 'model-a', project_id: '7' });
    const instructions = body.versions?.[0]?.instructions ?? '';
    expect(body.name).toBe('Local documents agent');
    expect(instructions).toContain('read_document');
    expect(instructions).toContain('(edited)');
    expect(instructions).toContain('can be undone');
    expect(instructions).toContain('never claim');
    expect(instructions).not.toContain('{{product}}');
  });
});
