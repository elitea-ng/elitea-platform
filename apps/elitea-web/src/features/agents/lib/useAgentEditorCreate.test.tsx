import { act } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { getCreateApplicationMockHandler } from '@/shared/api/generated/applications/applications.msw';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { renderHookWithProviders } from '../__tests__/testUtils';
import { useAgentEditorCreate } from './useAgentEditorCreate';

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
});

afterEach(() => {
  resetGeneratedClient();
});

describe('useAgentEditorCreate', () => {
  it('starts with empty create-mode values', () => {
    const { result } = renderHookWithProviders(() => useAgentEditorCreate('p1'));
    expect(result.current.values.name).toBe('');
    expect(result.current.values.version_details?.instructions).toBe('');
  });

  it('onFieldChange updates a top-level field', () => {
    const { result } = renderHookWithProviders(() => useAgentEditorCreate('p1'));

    act(() => result.current.onFieldChange('name', 'My Agent'));

    expect(result.current.values.name).toBe('My Agent');
  });

  it('onFieldChange updates a nested version_details field', () => {
    const { result } = renderHookWithProviders(() => useAgentEditorCreate('p1'));

    act(() => result.current.onFieldChange('version_details.instructions', 'Do things'));

    expect(result.current.values.version_details?.instructions).toBe('Do things');
  });

  it('submit creates the application with the trimmed name and current version fields', async () => {
    let capturedBody: unknown;
    server.use(
      getCreateApplicationMockHandler(async (info) => {
        capturedBody = await info.request.json();
        return {
          id: '42',
          name: 'My Agent',
          description: '',
          type: 'interface',
          icon: '',
          owner_id: 'u1',
          created_at: '2026-01-01T00:00:00Z',
        };
      }),
    );
    const { result } = renderHookWithProviders(() => useAgentEditorCreate('p1'));

    act(() => result.current.onFieldChange('name', '  My Agent  '));
    act(() => result.current.onFieldChange('version_details.instructions', 'Use {{topic}}'));

    let response;
    await act(async () => {
      response = await result.current.submit();
    });

    expect(response).toEqual(expect.objectContaining({ id: '42', name: 'My Agent' }));
    expect(capturedBody).toMatchObject({
      name: 'My Agent',
      versions: [expect.objectContaining({ instructions: 'Use {{topic}}' })],
    });
  });

  it('submit defaults meta.internal_tools to empty so the runtime admits the version (agent_chat.sql:359-362 / internal_tools.rs:47-61)', async () => {
    let capturedBody: unknown;
    server.use(
      getCreateApplicationMockHandler(async (info) => {
        capturedBody = await info.request.json();
        return {
          id: '42',
          name: 'My Agent',
          description: '',
          type: 'interface',
          icon: '',
          owner_id: 'u1',
          created_at: '2026-01-01T00:00:00Z',
        };
      }),
    );
    const { result } = renderHookWithProviders(() => useAgentEditorCreate('p1'));

    act(() => result.current.onFieldChange('name', 'My Agent'));
    await act(async () => {
      await result.current.submit();
    });

    expect(capturedBody).toMatchObject({
      versions: [{ meta: { internal_tools: [] } }],
    });
  });

  it('submit respects an explicit meta.internal_tools override over the default (a deliberate Elitea MCP Tools opt-in still reaches the wire)', async () => {
    let capturedBody: unknown;
    server.use(
      getCreateApplicationMockHandler(async (info) => {
        capturedBody = await info.request.json();
        return {
          id: '42',
          name: 'My Agent',
          description: '',
          type: 'interface',
          icon: '',
          owner_id: 'u1',
          created_at: '2026-01-01T00:00:00Z',
        };
      }),
    );
    const { result } = renderHookWithProviders(() => useAgentEditorCreate('p1'));

    act(() => result.current.onFieldChange('name', 'My Agent'));
    act(() => result.current.onFieldChange('version_details.meta.internal_tools', ['internal_mcp']));
    await act(async () => {
      await result.current.submit();
    });

    expect(capturedBody).toMatchObject({
      versions: [{ meta: { internal_tools: ['internal_mcp'] } }],
    });
  });

  it('submit sends the Conversation history block the field wrote, and no key for the default (ADR-0050 decision 9)', async () => {
    const bodies: unknown[] = [];
    server.use(
      getCreateApplicationMockHandler(async (info) => {
        bodies.push(await info.request.json());
        return {
          id: '42',
          name: 'My Agent',
          description: '',
          type: 'interface',
          icon: '',
          owner_id: 'u1',
          created_at: '2026-01-01T00:00:00Z',
        };
      }),
    );
    const { result } = renderHookWithProviders(() => useAgentEditorCreate('p1'));

    act(() => result.current.onFieldChange('name', 'My Agent'));
    await act(async () => {
      await result.current.submit();
    });
    act(() => result.current.onFieldChange('version_details.meta.context', { history: 'addressed_only', include_side_talk: 'none' }));
    await act(async () => {
      await result.current.submit();
    });

    const metaOf = (body: unknown) => (body as { versions: { meta: Record<string, unknown> }[] }).versions[0]?.meta;
    expect(metaOf(bodies[0])).not.toHaveProperty('context');
    expect(metaOf(bodies[1])).toMatchObject({ context: { history: 'addressed_only', include_side_talk: 'none' } });
  });

  it('starts clean and becomes dirty once a field changes', () => {
    const { result } = renderHookWithProviders(() => useAgentEditorCreate('p1'));
    expect(result.current.isDirty).toBe(false);

    act(() => result.current.onFieldChange('name', 'My Agent'));

    expect(result.current.isDirty).toBe(true);
  });

  it('reset restores the empty values and clears the dirty flag', () => {
    const { result } = renderHookWithProviders(() => useAgentEditorCreate('p1'));

    act(() => result.current.onFieldChange('name', 'My Agent'));
    expect(result.current.isDirty).toBe(true);

    act(() => result.current.reset());

    expect(result.current.values.name).toBe('');
    expect(result.current.isDirty).toBe(false);
  });

  it('submit clears the dirty flag on success', async () => {
    server.use(
      getCreateApplicationMockHandler(() => ({
        id: '42',
        name: 'My Agent',
        description: '',
        type: 'interface',
        icon: '',
        owner_id: 'u1',
        created_at: '2026-01-01T00:00:00Z',
      })),
    );
    const { result } = renderHookWithProviders(() => useAgentEditorCreate('p1'));

    act(() => result.current.onFieldChange('name', 'My Agent'));
    expect(result.current.isDirty).toBe(true);

    await act(async () => {
      await result.current.submit();
    });

    expect(result.current.isDirty).toBe(false);
  });
});
