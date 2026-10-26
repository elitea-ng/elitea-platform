import type { RefObject } from 'react';
import { act, waitFor } from '@testing-library/react';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import type { Participant } from '@/entities/participant';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { renderHookWithProviders } from '../../__tests__/testUtils';
import type { ChatInputHandle } from '../chatInputHandle';

import { useChatSkillMention } from './useChatSkillMention';

const BASE = '/api/v2';

class FakeChatInput implements ChatInputHandle {
  content = '';
  cursor: number | null = null;

  getInputContent(): string {
    return this.content;
  }

  getCursorPosition(): number | null {
    return this.cursor;
  }

  replaceRange(start: number, end: number, replacement: string): void {
    this.content = this.content.slice(0, start) + replacement + this.content.slice(end);
    this.cursor = start + replacement.length;
  }
}

function makeRef(): RefObject<ChatInputHandle | null> {
  return { current: new FakeChatInput() };
}

const AGENT_PARTICIPANT: Participant = {
  id: 'part-1',
  entityName: 'application',
  entityMeta: { id: 'app-1', projectId: 'proj-1' },
  entitySettings: { versionId: 42 },
  meta: { name: 'My Agent' },
};

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
});

afterEach(() => {
  resetGeneratedClient();
});

function mockSkills(items: readonly { id: string; name: string; description?: string }[]): void {
  server.use(
    http.get(`${BASE}/elitea_core/application_skills/prompt_lib/proj-1/42`, () =>
      HttpResponse.json({
        items: items.map((s) => ({ id: s.id, project_id: 'proj-1', name: s.name, description: s.description, type: 'skill', is_default: false, created_at: 't', updated_at: 't' })),
        total: items.length,
        page: 0,
        page_size: 20,
        total_pages: 1,
      }),
    ),
  );
}

describe('useChatSkillMention', () => {
  it('does not fetch skills when the active participant is not an agent', () => {
    const chatInput = makeRef();
    const nonAgent: Participant = { id: 'part-2', entityName: 'toolkit', entityMeta: { id: 'tk-1', projectId: 'proj-1' } };
    const { result } = renderHookWithProviders(() => useChatSkillMention({ chatInput, activeParticipant: nonAgent }));
    expect(result.current.filteredItems).toEqual([]);
  });

  it('loads and sorts the agent version’s skills alphabetically', async () => {
    mockSkills([
      { id: 's-2', name: 'Zeta', description: 'z' },
      { id: 's-1', name: 'Alpha', description: 'a' },
    ]);
    const chatInput = makeRef();
    const { result } = renderHookWithProviders(() => useChatSkillMention({ chatInput, activeParticipant: AGENT_PARTICIPANT }));

    await waitFor(() => expect(result.current.filteredItems).toHaveLength(0)); // idle: nothing typed yet, filteredItems mirrors idle mentionableItems only once "~"-triggered

    act(() => result.current.onSkillInputChange('~'));
    await waitFor(() => expect(result.current.filteredItems.map((i) => i.name)).toEqual(['Alpha', 'Zeta']));
    expect(result.current.skillPhase).toBe('items');
  });

  it('falls back to fallbackAppVersionId when entitySettings.versionId is absent', async () => {
    mockSkills([{ id: 's-1', name: 'Alpha' }]);
    const chatInput = makeRef();
    const participantNoVersion: Participant = { id: 'part-3', entityName: 'application', entityMeta: { id: 'app-1', projectId: 'proj-1' }, meta: { name: 'Agent' } };
    const { result } = renderHookWithProviders(() =>
      useChatSkillMention({ chatInput, activeParticipant: participantNoVersion, fallbackAppVersionId: 42 }),
    );
    act(() => result.current.onSkillInputChange('~'));
    await waitFor(() => expect(result.current.filteredItems.map((i) => i.name)).toEqual(['Alpha']));
  });

  it('falls back to fallbackAppVersionId when entitySettings.versionId is 0 (falsy-check parity, not nullish — the agent-editor test-chat scenario)', async () => {
    mockSkills([{ id: 's-1', name: 'Alpha' }]);
    const chatInput = makeRef();
    const participantVersionZero: Participant = {
      id: 'part-4',
      entityName: 'application',
      entityMeta: { id: 'app-1', projectId: 'proj-1' },
      entitySettings: { versionId: 0 },
      meta: { name: 'Agent' },
    };
    const { result } = renderHookWithProviders(() =>
      useChatSkillMention({ chatInput, activeParticipant: participantVersionZero, fallbackAppVersionId: 42 }),
    );
    act(() => result.current.onSkillInputChange('~'));
    await waitFor(() => expect(result.current.filteredItems.map((i) => i.name)).toEqual(['Alpha']));
  });

  it('detects "~query" only when preceded by whitespace/start-of-text, and filters by query', async () => {
    mockSkills([{ id: 's-1', name: 'Alpha' }, { id: 's-2', name: 'Beta' }]);
    const chatInput = makeRef();
    const { result } = renderHookWithProviders(() => useChatSkillMention({ chatInput, activeParticipant: AGENT_PARTICIPANT }));

    act(() => result.current.onSkillInputChange('x~al'));
    expect(result.current.skillPhase).toBe('idle');

    act(() => result.current.onSkillInputChange('~al'));
    await waitFor(() => expect(result.current.skillPhase).toBe('items'));
    await waitFor(() => expect(result.current.filteredItems.map((i) => i.name)).toEqual(['Alpha']));
  });

  it('onSelectSkill replaces the "~query" fragment and resets', async () => {
    mockSkills([{ id: 's-1', name: 'Alpha' }]);
    const chatInput = makeRef();
    const handle = chatInput.current as FakeChatInput;
    handle.content = '~al';
    handle.cursor = 3;
    const { result } = renderHookWithProviders(() => useChatSkillMention({ chatInput, activeParticipant: AGENT_PARTICIPANT }));

    act(() => result.current.onSkillInputChange('~al'));
    await waitFor(() => expect(result.current.filteredItems).toHaveLength(1));

    act(() => result.current.onSelectSkill(result.current.filteredItems[0]!));
    expect(handle.content).toBe('~Alpha ');
    expect(result.current.skillPhase).toBe('idle');
  });

  it('ArrowDown/ArrowUp/Enter navigate and select via onSkillKeyDown', async () => {
    mockSkills([{ id: 's-1', name: 'Alpha' }, { id: 's-2', name: 'Beta' }]);
    const chatInput = makeRef();
    const handle = chatInput.current as FakeChatInput;
    handle.content = '~';
    handle.cursor = 1;
    const { result } = renderHookWithProviders(() => useChatSkillMention({ chatInput, activeParticipant: AGENT_PARTICIPANT }));

    act(() => result.current.onSkillInputChange('~'));
    await waitFor(() => expect(result.current.filteredItems).toHaveLength(2));

    const preventDefault = () => {};
    act(() => result.current.onSkillKeyDown({ key: 'ArrowDown', preventDefault }));
    expect(result.current.highlightedIndex).toBe(1);
    act(() => result.current.onSkillKeyDown({ key: 'Enter', preventDefault }));
    expect(handle.content).toBe('~Beta ');
    expect(result.current.skillPhase).toBe('idle');
  });

  it('Escape resets the mention', async () => {
    mockSkills([{ id: 's-1', name: 'Alpha' }]);
    const chatInput = makeRef();
    const { result } = renderHookWithProviders(() => useChatSkillMention({ chatInput, activeParticipant: AGENT_PARTICIPANT }));
    act(() => result.current.onSkillInputChange('~a'));
    await waitFor(() => expect(result.current.filteredItems).toHaveLength(1));
    act(() => result.current.onSkillKeyDown({ key: 'Escape', preventDefault: () => {} }));
    expect(result.current.skillPhase).toBe('idle');
  });

  it('computes skillHighlightRanges and committedMentions from the input text', async () => {
    mockSkills([{ id: 's-1', name: 'Alpha' }]);
    const chatInput = makeRef();
    const { result } = renderHookWithProviders(() => useChatSkillMention({ chatInput, activeParticipant: AGENT_PARTICIPANT }));

    // Seed mentionableItems by opening the dropdown once, then simulate the
    // final committed text (as if a selection already replaced the fragment).
    act(() => result.current.onSkillInputChange('~a'));
    await waitFor(() => expect(result.current.filteredItems).toHaveLength(1));
    act(() => result.current.onSkillInputChange('~Alpha hello'));

    expect(result.current.skillHighlightRanges).toEqual([{ start: 0, end: 6 }]);
    expect(result.current.committedMentions).toEqual([{ name: 'Alpha' }]);
  });

  describe('project skills', () => {
    function mockProjectSkills(items: readonly { id: number; name: string; description?: string }[], seen?: { count: number }): void {
      server.use(
        http.get(`${BASE}/elitea_core/skills/prompt_lib/proj-1`, () => {
          if (seen) seen.count += 1;
          return HttpResponse.json({ items, total: items.length, page: 1, page_size: 100, total_pages: 1 });
        }),
      );
    }

    it('lists the version’s skills first, then the project’s others, leaving out names the version carries', async () => {
      mockSkills([{ id: 's-1', name: 'Alpha' }, { id: 's-2', name: 'Zeta' }]);
      mockProjectSkills([{ id: 9, name: 'Triage', description: 't' }, { id: 7, name: 'alpha' }, { id: 8, name: 'Build' }]);
      const chatInput = makeRef();
      const { result } = renderHookWithProviders(() => useChatSkillMention({ chatInput, activeParticipant: AGENT_PARTICIPANT, projectId: 'proj-1' }));

      act(() => result.current.onSkillInputChange('~'));
      await waitFor(() => expect(result.current.filteredItems.map((i) => i.name)).toEqual(['Alpha', 'Zeta', 'Build', 'Triage']));
      expect(result.current.filteredItems.map((i) => i.isProject)).toEqual([false, false, true, true]);
    });

    it('reads the project’s skills when an agent’s composer mounts, so a pasted ~Name resolves without the menu', async () => {
      mockSkills([{ id: 's-1', name: 'Alpha' }]);
      const seen = { count: 0 };
      mockProjectSkills([{ id: 9, name: 'Triage' }], seen);
      const chatInput = makeRef();
      const { result } = renderHookWithProviders(() => useChatSkillMention({ chatInput, activeParticipant: AGENT_PARTICIPANT, projectId: 'proj-1' }));

      await waitFor(() => expect(seen.count).toBe(1));
      expect(result.current.skillPhase).toBe('idle');
      await waitFor(() => expect(result.current.projectSkillIdsFor('please ~Triage this')).toEqual(['9']));
    });

    it('answers null for a ~ while the project’s skills are not read, and resolveProjectSkillIds reads them', async () => {
      mockSkills([]);
      server.use(http.get(`${BASE}/elitea_core/skills/prompt_lib/proj-1`, () => HttpResponse.json({ error: 'down' }, { status: 500 })));
      const chatInput = makeRef();
      const { result } = renderHookWithProviders(() => useChatSkillMention({ chatInput, activeParticipant: AGENT_PARTICIPANT, projectId: 'proj-1' }));

      expect(result.current.projectSkillIdsFor('no skills named here')).toEqual([]);
      expect(result.current.projectSkillIdsFor('use ~Triage')).toBeNull();
      await expect(result.current.resolveProjectSkillIds('use ~Triage')).rejects.toBeDefined();

      mockProjectSkills([{ id: 9, name: 'Triage' }]);
      await expect(result.current.resolveProjectSkillIds('use ~Triage')).resolves.toEqual(['9']);
    });

    it('offers no project skills to a participant that is not an agent', () => {
      const seen = { count: 0 };
      mockProjectSkills([{ id: 9, name: 'Triage' }], seen);
      const chatInput = makeRef();
      const nonAgent: Participant = { id: 'part-2', entityName: 'toolkit', entityMeta: { id: 'tk-1', projectId: 'proj-1' } };
      const { result } = renderHookWithProviders(() => useChatSkillMention({ chatInput, activeParticipant: nonAgent, projectId: 'proj-1' }));
      act(() => result.current.onSkillInputChange('~'));
      expect(result.current.filteredItems).toEqual([]);
      expect(seen.count).toBe(0);
    });

    it('projectSkillIdsFor answers the ids of the project skills the text mentions, once each, and nothing for the version’s', async () => {
      mockSkills([{ id: 's-1', name: 'Alpha' }]);
      mockProjectSkills([{ id: 9, name: 'Triage' }, { id: 8, name: 'Build' }]);
      const chatInput = makeRef();
      const { result } = renderHookWithProviders(() => useChatSkillMention({ chatInput, activeParticipant: AGENT_PARTICIPANT, projectId: 'proj-1' }));
      act(() => result.current.onSkillInputChange('~'));
      await waitFor(() => expect(result.current.filteredItems).toHaveLength(3));

      expect(result.current.projectSkillIdsFor('use ~Alpha then ~Triage and ~Triage, also ~Build')).toEqual(['9', '8']);
      expect(result.current.projectSkillIdsFor('only ~Alpha')).toEqual([]);
      expect(result.current.projectSkillIdsFor('~Unknown and Triage without a sigil')).toEqual([]);
    });
  });
});
