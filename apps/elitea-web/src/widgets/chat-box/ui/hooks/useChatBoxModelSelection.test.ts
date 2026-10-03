import { act, renderHook } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import './__mocks__/useChatBoxModelSelection.mock';
import { useChatBoxModelSelection } from './useChatBoxModelSelection';

describe('pipeline test model selection', () => {
  it('shows the configured model and project instead of the chat default', () => {
    const { result } = renderHook(() => useChatBoxModelSelection({
      projectId: '9', selectedModel: { name: 'default-model' }, setSelectedModel: vi.fn(),
      llm: { settings: { model_name: 'pipeline-model', model_project_id: '1' } },
    }));
    expect(result.current.selectedLlmModel?.id).toBe('shared');
  });

  it('updates the editor with the selected identity rather than changing only the chat model', () => {
    const setSelectedModel = vi.fn();
    const onSetSettings = vi.fn();
    const { result } = renderHook(() => useChatBoxModelSelection({
      projectId: '9', selectedModel: { name: 'default-model' }, setSelectedModel,
      llm: { settings: { model_name: 'pipeline-model', model_project_id: '1' }, onSetSettings },
    }));
    act(() => result.current.handleSelectModel({ id: 'private', name: 'pipeline-model' }));
    expect(onSetSettings).toHaveBeenCalledWith({ model_name: 'pipeline-model', model_project_id: '9' });
    expect(setSelectedModel).not.toHaveBeenCalled();
  });

  it('does not substitute a same-named model from another project', () => {
    const { result } = renderHook(() => useChatBoxModelSelection({
      projectId: '9', selectedModel: { name: 'default-model' }, setSelectedModel: vi.fn(),
      llm: { settings: { model_name: 'pipeline-model', model_project_id: '8' } },
    }));
    expect(result.current.selectedLlmModel).toBeNull();
  });

  it('retains model selection for ordinary chat', () => {
    const setSelectedModel = vi.fn();
    const { result } = renderHook(() => useChatBoxModelSelection({
      projectId: '9', selectedModel: { name: 'default-model' }, setSelectedModel,
    }));
    expect(result.current.selectedLlmModel?.id).toBe('default');
    act(() => result.current.handleSelectModel({ id: 'shared', name: 'pipeline-model' }));
    expect(setSelectedModel).toHaveBeenCalledWith({ name: 'pipeline-model', projectId: '1', supportsReasoning: false });
  });

  it('shows a default model shared from project 1 when no model is configured (UI-PD-2)', () => {
    const { result } = renderHook(() => useChatBoxModelSelection({
      projectId: '9', selectedModel: { name: 'pipeline-model', projectId: '1' }, setSelectedModel: vi.fn(),
      llm: { settings: { temperature: 0.6 } },
    }));
    expect(result.current.selectedLlmModel?.id).toBe('shared');
  });

  it('ignores a stray current-project id for the implied default (UI-PD-2)', () => {
    const { result } = renderHook(() => useChatBoxModelSelection({
      projectId: '9', selectedModel: { name: 'pipeline-model', projectId: '1' }, setSelectedModel: vi.fn(),
      llm: { settings: { model_project_id: 9 } },
    }));
    expect(result.current.selectedLlmModel?.id).toBe('shared');
  });

  it('prefers the chat project model for a configured name saved with no project id', () => {
    // The include_shared list puts the shared copy first; the server resolves
    // a project-less name in the chat project, so the picker must show that one.
    const { result } = renderHook(() => useChatBoxModelSelection({
      projectId: '9', selectedModel: { name: 'default-model' }, setSelectedModel: vi.fn(),
      llm: { settings: { model_name: 'pipeline-model' } },
    }));
    expect(result.current.selectedLlmModel?.id).toBe('private');
    expect(result.current.configuredModelProjectId).toBe(9);
  });

  it('falls back to a shared model only when the chat project has none, and names its project', () => {
    const { result } = renderHook(() => useChatBoxModelSelection({
      projectId: '5', selectedModel: { name: 'default-model' }, setSelectedModel: vi.fn(),
      llm: { settings: { model_name: 'pipeline-model' } },
    }));
    expect(result.current.selectedLlmModel?.id).toBe('shared');
    expect(result.current.configuredModelProjectId).toBe(1);
  });

  it('adds no project when the configured model already names one', () => {
    const { result } = renderHook(() => useChatBoxModelSelection({
      projectId: '9', selectedModel: { name: 'default-model' }, setSelectedModel: vi.fn(),
      llm: { settings: { model_name: 'pipeline-model', model_project_id: '1' } },
    }));
    expect(result.current.configuredModelProjectId).toBeUndefined();
  });

  it('refuses an object project identity without invoking its string conversion', () => {
    const toString = vi.fn(() => '1');
    const { result } = renderHook(() => useChatBoxModelSelection({
      projectId: '9', selectedModel: { name: 'default-model' }, setSelectedModel: vi.fn(),
      llm: { settings: { model_name: 'pipeline-model', model_project_id: { toString } } },
    }));
    expect(result.current.selectedLlmModel).toBeNull();
    expect(toString).not.toHaveBeenCalled();
  });
});
