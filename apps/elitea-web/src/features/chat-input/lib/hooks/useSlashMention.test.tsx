import type { RefObject } from 'react';
import { act, waitFor } from '@testing-library/react';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import type { Participant } from '@/entities/participant';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { renderHookWithProviders } from '../../__tests__/testUtils';
import type { ChatInputHandle } from '../chatInputHandle';

import { useSlashMention } from './useSlashMention';

const BASE = '/api/v2';

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
  server.use(
    http.get(`${BASE}/elitea_core/platform_settings/prompt_lib`, () =>
      HttpResponse.json({
        chat_enabled: true,
        applications_enabled: true,
        skills_enabled: true,
        toolkits_enabled: true,
        datasources_enabled: true,
        pipelines_enabled: true,
        publishing_enabled: true,
        moderation_enabled: true,
        support_chat_enabled: true,
        mcp_enabled: true,
      }),
    ),
  );
});

afterEach(() => {
  resetGeneratedClient();
});

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

function toolkitParticipant(overrides: Partial<Participant> = {}): Participant {
  return {
    id: 'part-1',
    entityName: 'toolkit',
    entityMeta: { id: 'tk-1', projectId: 'proj-1' },
    entitySettings: { toolkitType: 'github' },
    meta: { name: 'GitHub' },
    ...overrides,
  };
}

function makeRef(): RefObject<ChatInputHandle | null> {
  return { current: new FakeChatInput() };
}

describe('useSlashMention', () => {
  it('starts idle with no participant toolkits when participants is undefined', () => {
    const chatInput = makeRef();
    const { result } = renderHookWithProviders(() => useSlashMention({ chatInput, participants: undefined }));
    expect(result.current.phase).toBe('idle');
    expect(result.current.participantToolkits).toEqual([]);
  });

  it('derives participantToolkits from toolkit-entityName participants only', () => {
    const chatInput = makeRef();
    const participants: Participant[] = [
      toolkitParticipant(),
      { id: 'part-2', entityName: 'user', entityMeta: { id: 'u-1', projectId: 'proj-1' }, meta: { name: 'Alice' } },
    ];
    const { result } = renderHookWithProviders(() => useSlashMention({ chatInput, participants }));
    expect(result.current.participantToolkits).toEqual([{ id: 'tk-1', projectId: 'proj-1', type: 'github', name: 'GitHub' }]);
  });

  it('reads an iconUrl off entitySettings.iconMeta.url when present', () => {
    const chatInput = makeRef();
    const participants: Participant[] = [toolkitParticipant({ entitySettings: { toolkitType: 'github', iconMeta: { url: 'https://x/y.png' } } })];
    const { result } = renderHookWithProviders(() => useSlashMention({ chatInput, participants }));
    expect(result.current.participantToolkits[0]?.iconUrl).toBe('https://x/y.png');
  });

  it('excludes MCP toolkits when the platform reports mcp_enabled: false', async () => {
    server.use(
      http.get(`${BASE}/elitea_core/platform_settings/prompt_lib`, () =>
        HttpResponse.json({
          chat_enabled: true,
          applications_enabled: true,
          skills_enabled: true,
          toolkits_enabled: true,
          datasources_enabled: true,
          pipelines_enabled: true,
          publishing_enabled: true,
          moderation_enabled: true,
          support_chat_enabled: true,
          mcp_enabled: false,
        }),
      ),
    );
    const chatInput = makeRef();
    const participants: Participant[] = [
      toolkitParticipant(),
      toolkitParticipant({ id: 'part-3', entityMeta: { id: 'tk-2', projectId: 'proj-1' }, entitySettings: { toolkitType: 'mcp' }, meta: { name: 'My MCP' } }),
    ];
    const { result } = renderHookWithProviders(() => useSlashMention({ chatInput, participants }));
    await waitFor(() => expect(result.current.isMcpVisible).toBe(false));
    expect(result.current.participantToolkits.map((t) => t.id)).toEqual(['tk-1']);
  });

  it('onSelectToolkit replaces the "/query" fragment with "/toolkitName" and advances to tool phase', () => {
    const chatInput = makeRef();
    const handle = chatInput.current as FakeChatInput;
    handle.content = '/git';
    handle.cursor = 4;
    const participants: Participant[] = [toolkitParticipant()];
    const { result } = renderHookWithProviders(() => useSlashMention({ chatInput, participants }));

    act(() => result.current.onInputChange('/git'));
    act(() => result.current.onSelectToolkit(result.current.participantToolkits[0]!));

    expect(handle.content).toBe('/GitHub');
    expect(result.current.phase).toBe('tool');
    expect(result.current.selectedToolkit).toEqual({ id: 'tk-1', projectId: 'proj-1', name: 'GitHub', type: 'github' });
  });

  it('onCommitMention writes the final "/toolkit/tool " token and resets to idle', () => {
    const chatInput = makeRef();
    const handle = chatInput.current as FakeChatInput;
    const participants: Participant[] = [toolkitParticipant()];
    const { result } = renderHookWithProviders(() => useSlashMention({ chatInput, participants }));

    act(() => result.current.onInputChange('/git'));
    act(() => result.current.onSelectToolkit(result.current.participantToolkits[0]!));
    act(() => result.current.onCommitMention('create_issue'));

    expect(handle.content).toBe('/GitHub/create_issue ');
    expect(result.current.phase).toBe('idle');
    expect(result.current.committedMentions).toEqual([
      { toolkitId: 'tk-1', projectId: 'proj-1', toolkitName: 'GitHub', toolkitType: 'github', toolName: 'create_issue' },
    ]);
  });

  it('onSelectSkillsEntry writes "/skill " over the typed fragment, closes the list and says where the cursor is', () => {
    const chatInput = makeRef();
    const handle = chatInput.current as FakeChatInput;
    handle.content = '/sk';
    handle.cursor = 3;
    const { result } = renderHookWithProviders(() => useSlashMention({ chatInput, participants: [] }));

    act(() => result.current.onInputChange('/sk'));
    expect(result.current.phase).toBe('toolkit');
    let inserted: ReturnType<typeof result.current.onSelectSkillsEntry> = null;
    act(() => {
      inserted = result.current.onSelectSkillsEntry();
    });

    expect(handle.content).toBe('/skill ');
    expect(inserted).toEqual({ text: '/skill ', cursor: 7 });
    expect(result.current.phase).toBe('idle');
  });

  it('onSelectSkillsEntry writes "~" when the "/" is not at the start of a line, where "/skill" would not open the picker', () => {
    const chatInput = makeRef();
    const handle = chatInput.current as FakeChatInput;
    handle.content = 'use /';
    handle.cursor = 5;
    const { result } = renderHookWithProviders(() => useSlashMention({ chatInput, participants: [] }));

    act(() => result.current.onInputChange('use /'));
    let inserted: ReturnType<typeof result.current.onSelectSkillsEntry> = null;
    act(() => {
      inserted = result.current.onSelectSkillsEntry();
    });

    expect(handle.content).toBe('use ~');
    expect(inserted).toEqual({ text: 'use ~', cursor: 5 });
  });

  it('onInputChange("") clears mentions and resets', () => {
    const chatInput = makeRef();
    const participants: Participant[] = [toolkitParticipant()];
    const { result } = renderHookWithProviders(() => useSlashMention({ chatInput, participants }));

    act(() => result.current.onInputChange('/git'));
    act(() => result.current.onSelectToolkit(result.current.participantToolkits[0]!));
    act(() => result.current.onInputChange(''));

    expect(result.current.phase).toBe('idle');
    expect(result.current.committedMentions).toEqual([]);
  });

  it('highlightRanges reflects committed mentions in the current input text', () => {
    const chatInput = makeRef();
    const handle = chatInput.current as FakeChatInput;
    const participants: Participant[] = [toolkitParticipant()];
    const { result } = renderHookWithProviders(() => useSlashMention({ chatInput, participants }));

    act(() => result.current.onInputChange('/git'));
    act(() => result.current.onSelectToolkit(result.current.participantToolkits[0]!));
    act(() => result.current.onCommitMention(null));

    expect(handle.content).toBe('/GitHub ');
    expect(result.current.highlightRanges).toEqual([{ start: 0, end: 7 }]);
  });
});
