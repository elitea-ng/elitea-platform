import { act } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { getCreateApplicationMockHandler } from '@/shared/api/generated/applications/applications.msw';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { renderHookWithProviders } from '../__tests__/testUtils';
import { PIPELINE_STARTER_TEMPLATE } from '@/shared/lib/pipelineStarterTemplate';
import { usePipelineEditorCreate } from './usePipelineEditorCreate';

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
});

afterEach(() => {
  resetGeneratedClient();
});

describe('usePipelineEditorCreate', () => {
  it('starts with empty create-mode values and the starter graph', () => {
    const { result } = renderHookWithProviders(() => usePipelineEditorCreate('p1'));
    expect(result.current.values.name).toBe('');
    // `instructions` IS the graph. It used to start empty, which stored a
    // pipeline no runtime could run. See `./pipelineStarterTemplate.ts`.
    expect(result.current.values.version_details?.instructions).toBe(PIPELINE_STARTER_TEMPLATE);
  });

  it('onFieldChange updates a top-level field', () => {
    const { result } = renderHookWithProviders(() => usePipelineEditorCreate('p1'));

    act(() => result.current.onFieldChange('name', 'My Pipeline'));

    expect(result.current.values.name).toBe('My Pipeline');
  });

  it('onFieldChange updates a nested version_details field', () => {
    const { result } = renderHookWithProviders(() => usePipelineEditorCreate('p1'));

    act(() => result.current.onFieldChange('version_details.instructions', 'state:\n  input:\n    type: str\n'));

    expect(result.current.values.version_details?.instructions).toBe('state:\n  input:\n    type: str\n');
  });

  it('submit creates the application with agent_type "pipeline" and pipeline_settings seeded empty', async () => {
    let capturedBody: unknown;
    server.use(
      getCreateApplicationMockHandler(async (info) => {
        capturedBody = await info.request.json();
        return {
          id: '42',
          name: 'My Pipeline',
          description: '',
          type: 'interface',
          icon: '',
          owner_id: 'u1',
          created_at: '2026-01-01T00:00:00Z',
        };
      }),
    );
    const { result } = renderHookWithProviders(() => usePipelineEditorCreate('p1'));

    act(() => result.current.onFieldChange('name', '  My Pipeline  '));

    let response;
    await act(async () => {
      response = await result.current.submit();
    });

    expect(response).toEqual(expect.objectContaining({ id: '42', name: 'My Pipeline' }));
    expect(capturedBody).toMatchObject({
      name: 'My Pipeline',
      versions: [expect.objectContaining({ agent_type: 'pipeline' })],
    });
    // The graph reaches the wire, so the created pipeline runs as created.
    expect(capturedBody).toMatchObject({
      versions: [{ instructions: PIPELINE_STARTER_TEMPLATE }],
    });
  });

  it('submit defaults meta.internal_tools to empty so the runtime admits the version (agent_chat.sql:359-362 / internal_tools.rs:47-61)', async () => {
    let capturedBody: unknown;
    server.use(
      getCreateApplicationMockHandler(async (info) => {
        capturedBody = await info.request.json();
        return {
          id: '42',
          name: 'My Pipeline',
          description: '',
          type: 'interface',
          icon: '',
          owner_id: 'u1',
          created_at: '2026-01-01T00:00:00Z',
        };
      }),
    );
    const { result } = renderHookWithProviders(() => usePipelineEditorCreate('p1'));

    act(() => result.current.onFieldChange('name', 'My Pipeline'));
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
          name: 'My Pipeline',
          description: '',
          type: 'interface',
          icon: '',
          owner_id: 'u1',
          created_at: '2026-01-01T00:00:00Z',
        };
      }),
    );
    const { result } = renderHookWithProviders(() => usePipelineEditorCreate('p1'));

    act(() => result.current.onFieldChange('name', 'My Pipeline'));
    act(() => result.current.onFieldChange('version_details.meta.internal_tools', ['internal_mcp']));
    await act(async () => {
      await result.current.submit();
    });

    expect(capturedBody).toMatchObject({
      versions: [{ meta: { internal_tools: ['internal_mcp'] } }],
    });
  });
});
