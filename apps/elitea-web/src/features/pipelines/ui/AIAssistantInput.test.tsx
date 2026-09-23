import type { ReactElement, ReactNode } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { server } from '../../../test/setup';
import { installWebStorageShim } from '../../../test/webstorage';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';
import { installCodeMirrorTestPolyfills } from '@/shared/ui/lib/field/codeMirrorTestPolyfills';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { SocketClientContext } from '@/shared/api/socket/client';
import { createTestSocketClient } from '@/shared/api/socket/testing';

import { AIAssistantInput } from './AIAssistantInput';
import type { AIAssistantInputProps } from './AIAssistantInput';
import { resetBackendCapabilitiesForTests, setBackendCapabilityForTests } from '@/shared/config/backendCapabilities';

// The AI Assistant trigger POSTs `predict_llm`, which the Go router mounts in
// no profile, so `AIAssistantInput` hides it by default. See
// `shared/config/backendCapabilities`. These tests assert the trigger's own
// behaviour, so they turn the capability on.
beforeEach(() => {
  setBackendCapabilityForTests('llmPredictStreaming', true);
});

afterEach(() => {
  resetBackendCapabilitiesForTests();
});


installCodeMirrorTestPolyfills();
installWebStorageShim();

const BASE = '/api/v2';

function stubConfigurationsEndpoints(): void {
  server.use(
    http.get(`${BASE}/configurations/available/`, () => HttpResponse.json([])),
    http.get(`${BASE}/configurations/configurations/7`, () => HttpResponse.json({ items: [], total: 0 })),
  );
}

function renderInput(props: AIAssistantInputProps): ReturnType<typeof renderWithTheme> {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const client = createTestSocketClient();
  function Wrapper({ children }: { children: ReactNode }): ReactElement {
    return (
      <QueryClientProvider client={queryClient}>
        <SocketClientContext.Provider value={client}>{children as ReactElement}</SocketClientContext.Provider>
      </QueryClientProvider>
    );
  }
  return renderWithTheme(
    <Wrapper>
      <AIAssistantInput {...props} />
    </Wrapper>,
  );
}

afterEach(() => {
  resetGeneratedClient();
});

describe('AIAssistantInput', () => {
  it('renders the value inside an InputBase and no modal until opened', () => {
    const { getByDisplayValue, queryByPlaceholderText } = renderInput({ value: 'draft prompt' });
    expect(getByDisplayValue('draft prompt')).toBeInTheDocument();
    expect(queryByPlaceholderText('Describe your idea to generate or rewrite the value.')).toBeNull();
  });

  it('opens the AIAssistantModal when the trigger button is clicked', async () => {
    configureGeneratedClient({ baseUrl: BASE });
    stubConfigurationsEndpoints();
    const ui = renderInput({ value: 'draft prompt', fieldName: 'system', projectId: 7 });

    await userEvent.click(ui.getByRole('button', { name: 'AI Assistant' }));

    expect(ui.getByPlaceholderText('Describe your idea to generate or rewrite the value.')).toBeInTheDocument();
  });

  it('closes the modal via its close button', async () => {
    configureGeneratedClient({ baseUrl: BASE });
    stubConfigurationsEndpoints();
    const ui = renderInput({ value: 'draft prompt', fieldName: 'system', projectId: 7 });

    await userEvent.click(ui.getByRole('button', { name: 'AI Assistant' }));
    expect(ui.getByPlaceholderText('Describe your idea to generate or rewrite the value.')).toBeInTheDocument();

    await userEvent.click(ui.getByLabelText('Close'));
    expect(ui.queryByPlaceholderText('Describe your idea to generate or rewrite the value.')).toBeNull();
  });

  // Regression coverage for confirmed adversarial-review finding #1
  // (`AIAssistantInput.tsx:60`): typing directly into the inline field used
  // to be silently discarded because `buildInputBaseProps` forwarded no
  // change handler at all.
  it('forwards a typed keystroke to fieldBinding.onInput (the primary, non-AI editing path)', async () => {
    const onInput = vi.fn();
    const ui = renderInput({ value: 'draft', fieldBinding: { onInput } });

    await userEvent.type(ui.getByDisplayValue('draft'), '!');

    expect(onInput).toHaveBeenCalled();
    const lastEvent = onInput.mock.calls.at(-1)?.[0] as { target: { value: string } };
    expect(lastEvent.target.value).toBe('draft!');
  });

  it('routes typed keystrokes to fieldBinding.onChange instead of onInput when hasOnChangeCallback is set', async () => {
    const onChange = vi.fn();
    const onInput = vi.fn();
    const ui = renderInput({ value: 'draft', fieldBinding: { hasOnChangeCallback: true, onChange, onInput } });

    await userEvent.type(ui.getByDisplayValue('draft'), '!');

    expect(onChange).toHaveBeenCalled();
    expect(onInput).not.toHaveBeenCalled();
  });
  /*
   * elitea_issues: #5305 — moot for the same reason: "Input AI assistant ...
   * broken — 500 Internal Server Error" describes the legacy task-polling
   * transport (`GET`/`DELETE .../task/prompt_lib/...`), which this app never
   * calls (`aiAssistantPredict.ts` uses the streaming `predict_llm` +
   * `application_predict` socket event instead) — and the trigger that would
   * reach it is gated off below.
   *
   * THE GATE LIVES ON THIS COMPONENT, not only on its call sites.
   *
   * The modal posts to `/elitea_core/predict_llm/...`, which the Go router
   * registers in no profile, so the trigger can only ever 404.
   * `settings/SimpleLLMInputItem.tsx` was gated first, but `nodes/PrinterNode`,
   * `nodes/ConditionNode` and `settings/PipelineSettings` render this component
   * directly and kept a live button. This case pins the gate here, so a later
   * call site cannot reintroduce the 404 by forgetting its own check.
   */
  it('renders no AI Assistant trigger while the predict_llm backend is absent', () => {
    resetBackendCapabilitiesForTests();
    const ui = renderInput({ value: 'draft' });

    expect(ui.queryByRole('button', { name: 'AI Assistant' })).not.toBeInTheDocument();
    // The field itself must still work; only the trigger goes.
    expect(ui.getByDisplayValue('draft')).toBeInTheDocument();
  });
});
