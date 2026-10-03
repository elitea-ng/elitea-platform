import { act, renderHook } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import './__mocks__/useChatBoxModelSelection.mock';
import { useChatBoxModelSelection } from './useChatBoxModelSelection';

describe('pipeline test model selection', () => {
  it('shows the configured model and project instead of the chat default', () => {
    const { result } = renderHook(() => useChatBoxModelSelection({
      projectId: '9', selectedModelName: 'default-model', setSelectedModel: vi.fn(),
      llm: { settings: { model_name: 'pipeline-model', model_project_id: '1' } },
    }));
    expect(result.current.selectedLlmModel?.id).toBe('shared');
  });

  it('updates the editor with the selected identity rather than changing only the chat model', () => {
    const setSelectedModel = vi.fn();
    const onSetSettings = vi.fn();
    const { result } = renderHook(() => useChatBoxModelSelection({
      projectId: '9', selectedModelName: 'default-model', setSelectedModel,
      llm: { settings: { model_name: 'pipeline-model', model_project_id: '1' }, onSetSettings },
    }));
    act(() => result.current.handleSelectModel({ id: 'private', name: 'pipeline-model' }));
    expect(onSetSettings).toHaveBeenCalledWith({ model_name: 'pipeline-model', model_project_id: '9' });
    expect(setSelectedModel).not.toHaveBeenCalled();
  });

  it('does not substitute a same-named model from another project', () => {
    const { result } = renderHook(() => useChatBoxModelSelection({
      projectId: '9', selectedModelName: 'default-model', setSelectedModel: vi.fn(),
      llm: { settings: { model_name: 'pipeline-model', model_project_id: '8' } },
    }));
    expect(result.current.selectedLlmModel).toBeNull();
  });

  it('retains model selection for ordinary chat', () => {
    const setSelectedModel = vi.fn();
    const { result } = renderHook(() => useChatBoxModelSelection({
      projectId: '9', selectedModelName: 'default-model', setSelectedModel,
    }));
    expect(result.current.selectedLlmModel?.id).toBe('default');
    act(() => result.current.handleSelectModel({ id: 'shared', name: 'pipeline-model' }));
    expect(setSelectedModel).toHaveBeenCalledWith({ name: 'pipeline-model', projectId: '1', supportsReasoning: false });
  });

  it('shows a default model shared from project 1 when no model is configured (UI-PD-2)', () => {
    const { result } = renderHook(() => useChatBoxModelSelection({
      projectId: '9', selectedModelName: 'pipeline-model', selectedModelProjectId: '1', setSelectedModel: vi.fn(),
      llm: { settings: { temperature: 0.6 } },
    }));
    expect(result.current.selectedLlmModel?.id).toBe('shared');
  });

  it('ignores a stray current-project id for the implied default (UI-PD-2)', () => {
    const { result } = renderHook(() => useChatBoxModelSelection({
      projectId: '9', selectedModelName: 'pipeline-model', selectedModelProjectId: '1', setSelectedModel: vi.fn(),
      llm: { settings: { model_project_id: 9 } },
    }));
    expect(result.current.selectedLlmModel?.id).toBe('shared');
  });

  it('refuses an object project identity without invoking its string conversion', () => {
    const toString = vi.fn(() => '1');
    const { result } = renderHook(() => useChatBoxModelSelection({
      projectId: '9', selectedModelName: 'default-model', setSelectedModel: vi.fn(),
      llm: { settings: { model_name: 'pipeline-model', model_project_id: { toString } } },
    }));
    expect(result.current.selectedLlmModel).toBeNull();
    expect(toString).not.toHaveBeenCalled();
  });
});
