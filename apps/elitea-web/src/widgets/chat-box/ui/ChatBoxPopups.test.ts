/**
 * #6774: the "#" picker is wired, and a pick both attaches the entity and
 * removes the typed "#query" from the composer.
 */
import { describe, expect, it, vi } from 'vitest';

import { buildChatBoxPopupsProps } from './ChatBoxPopups';
import type { useChatBoxState } from './hooks/useChatBoxState';

type ChatBoxState = ReturnType<typeof useChatBoxState>;

function fakeState(isProcessingSymbols: boolean): { state: ChatBoxState; clearHashQuery: ReturnType<typeof vi.fn>; stop: ReturnType<typeof vi.fn> } {
  const clearHashQuery = vi.fn();
  const stop = vi.fn();
  const state = {
    showRecommendationList: false,
    setShowRecommendationList: vi.fn(),
    hasOtherUsers: false,
    users: [],
    isSkillPhaseActive: false,
    skill: { filteredItems: [], highlightedIndex: -1 },
    slash: { phase: 'idle' },
    clearHashQuery,
    keyDown: {
      isProcessingSymbols,
      query: '#rev',
      stopProcessingSymbols: stop,
      isProcessingAtSymbol: false,
      atQuery: '',
      stopProcessingAtSymbol: vi.fn(),
    },
  } as unknown as ChatBoxState;
  return { state, clearHashQuery, stop };
}

describe('buildChatBoxPopupsProps hash picker (#6774)', () => {
  it('opens while "#" is being processed and closes through the key handler', () => {
    const { state, stop } = fakeState(true);
    const props = buildChatBoxPopupsProps({
      state, onChangeParticipant: undefined, existingParticipants: [], projectId: '7',
      onSelectUser: vi.fn(), onSelectTool: vi.fn(), onAddParticipant: vi.fn(),
    });
    expect(props.hashPicker).toMatchObject({ isOpen: true, query: '#rev', projectId: '7' });
    props.hashPicker?.onClose();
    expect(stop).toHaveBeenCalled();
  });

  it('stays closed with no "#" query or no attach handler', () => {
    expect(buildChatBoxPopupsProps({
      state: fakeState(false).state, onChangeParticipant: undefined, existingParticipants: [], projectId: '7',
      onSelectUser: vi.fn(), onSelectTool: vi.fn(), onAddParticipant: vi.fn(),
    }).hashPicker?.isOpen).toBe(false);
    expect(buildChatBoxPopupsProps({
      state: fakeState(true).state, onChangeParticipant: undefined, existingParticipants: [], projectId: '7',
      onSelectUser: vi.fn(), onSelectTool: vi.fn(),
    }).hashPicker?.isOpen).toBe(false);
  });

  it('removes the typed query and attaches the picked entity', () => {
    const { state, clearHashQuery } = fakeState(true);
    const onAddParticipant = vi.fn();
    const props = buildChatBoxPopupsProps({
      state, onChangeParticipant: undefined, existingParticipants: [], projectId: '7',
      onSelectUser: vi.fn(), onSelectTool: vi.fn(), onAddParticipant,
    });
    const selection = { id: '11', name: 'Reviewer', participantType: 'application' };
    props.hashPicker?.onSelect(selection);
    expect(clearHashQuery).toHaveBeenCalled();
    expect(onAddParticipant).toHaveBeenCalledWith(selection);
  });
});
