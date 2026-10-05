import type { ReactElement, ReactNode } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import userEvent from '@testing-library/user-event';
import { fireEvent, waitFor, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { resetBackendCapabilitiesForTests, setBackendCapabilityForTests } from '@/shared/config/backendCapabilities';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';
import { installCodeMirrorTestPolyfills } from '@/shared/ui/lib/field/codeMirrorTestPolyfills';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { SocketClientContext } from '@/shared/api/socket/client';
import { createTestSocketClient } from '@/shared/api/socket/testing';
import { server } from '@/test/setup';
import { installWebStorageShim } from '@/test/webstorage';

import { SimpleLLMInputItem, type SimpleLLMInputItemProps } from './SimpleLLMInputItem';

installCodeMirrorTestPolyfills();
installWebStorageShim();

function renderItem(props: SimpleLLMInputItemProps): ReturnType<typeof renderWithTheme> {
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
      <SimpleLLMInputItem {...props} />
    </Wrapper>,
  );
}

beforeEach(() => {
  // The AI Assistant POSTs `predict_llm`, which no router mounts, so the
  // field renders as the plain input by default — see
  // `shared/config/backendCapabilities`.
  setBackendCapabilityForTests('llmPredictStreaming', true);
});

afterEach(() => {
  resetGeneratedClient();
  resetBackendCapabilitiesForTests();
});

describe('SimpleLLMInputItem', () => {
  it('renders a heading chip with the capitalized, underscore-stripped variable name', () => {
    const { getByText } = renderItem({
      variableName: 'user_message',
      variable: 'user_message',
      type: 'fixed',
      value: '',
      defaultValue: '',
      onChangeMapping: vi.fn(),
    });
    expect(getByText('User message')).toBeInTheDocument();
  });

  it('renders a plain text field for a fixed/fstring/string type', () => {
    const { getByLabelText } = renderItem({
      variableName: 'system',
      variable: 'system',
      type: 'fixed',
      value: 'hello',
      defaultValue: '',
      onChangeMapping: vi.fn(),
    });
    expect(getByLabelText('Value')).toHaveValue('hello');
  });

  it.each([
    { type: 'fixed', capability: false },
    { type: 'fstring', capability: false },
    { type: 'fixed', capability: true },
    { type: 'fstring', capability: true },
  ])('preserves multiline Code source for $type with AI capability $capability', ({ type, capability }) => {
    setBackendCapabilityForTests('llmPredictStreaming', capability);
    const source = 'def run(value):\n    return value + 1\n\nresult = run(input)\n';
    const edited = source.replace('value + 1', 'value + 2');
    let savedValue: unknown;
    const props = {
      variableName: 'code', variable: 'code', type, value: source, defaultValue: '', enableAIAssistant: true,
      onChangeMapping: vi.fn((_variable: string, mapping: { value: unknown }) => {
        savedValue = mapping.value;
      }),
    };
    const ui = renderItem(props);
    const field = ui.getByLabelText('Value');

    expect(field.tagName).toBe('TEXTAREA');
    expect(field).toHaveValue(source);
    fireEvent.change(field, { target: { value: edited } });
    expect(props.onChangeMapping).toHaveBeenLastCalledWith('code', { type, value: edited });
    ui.unmount();
    const reopened = renderItem({ ...props, value: savedValue });
    expect(reopened.getByLabelText('Value')).toHaveValue(edited);
  });

  it('preserves Code source through the existing fullscreen editor and reopen', async () => {
    setBackendCapabilityForTests('llmPredictStreaming', false);
    const user = userEvent.setup();
    const source = 'def run(value):\n\treturn value + 1\n\n';
    const edited = source + 'result = run(input)\n';
    let savedValue: unknown;
    const props = {
      variableName: 'code', variable: 'code', type: 'fixed', value: source, defaultValue: '',
      onChangeMapping: vi.fn((_variable: string, mapping: { value: unknown }) => {
        savedValue = mapping.value;
      }),
    };
    const ui = renderItem(props);
    expect(ui.getByLabelText('Value')).toHaveValue(source);
    await user.click(ui.getByRole('button', { name: 'Full screen view' }));
    const field = within(ui.getByRole('dialog')).getByLabelText('Value');
    expect(field.tagName).toBe('TEXTAREA');
    expect(field).toHaveValue(source);
    fireEvent.change(field, { target: { value: edited } });
    expect(savedValue).toBe(edited);
    await user.click(ui.getByRole('button', { name: 'Close' }));
    await waitFor(() => expect(ui.queryByRole('dialog')).toBeNull());
    ui.unmount();
    const reopened = renderItem({ ...props, value: savedValue });
    expect(reopened.getByLabelText('Value')).toHaveValue(edited);
  });

  it.each([false, true])('keeps an ordinary task input unchanged with AI capability %s', capability => {
    setBackendCapabilityForTests('llmPredictStreaming', capability);
    const { getByLabelText } = renderItem({
      variableName: 'task', variable: 'task', type: 'fixed', value: 'ordinary task', defaultValue: '',
      codeLanguage: 'rust', enableAIAssistant: true, onChangeMapping: vi.fn(),
    });
    const field = getByLabelText('Value');
    expect(field.tagName).toBe('INPUT');
    expect(field).toHaveValue('ordinary task');
  });

  it('renders a Value select (not a text field) for a variable type', () => {
    const { queryByRole, getByRole } = renderItem({
      variableName: 'code',
      variable: 'code',
      type: 'variable',
      value: 'input',
      defaultValue: '',
      onChangeMapping: vi.fn(),
    });
    expect(queryByRole('textbox', { name: 'Value' })).not.toBeInTheDocument();
    expect(getByRole('combobox', { name: 'Value' })).toBeInTheDocument();
  });

  /*
   * React Flow's escape hatch, pinned because losing it is INVISIBLE to every
   * other test in this file: the canvas's drag layer swallows the mouse-down,
   * so a click on either dropdown focuses the combobox and never opens its
   * menu, while keyboard interaction — which is what these tests use — keeps
   * working. It was measured on the live stack as a combobox `[active]` with
   * no `listbox` in the accessibility tree.
   *
   * The class is asserted on an ANCESTOR rather than the control itself:
   * `hasSelector` (@xyflow/system) walks up from the event target, and
   * `SingleSelect` is shared and already at the 12-prop budget, so it cannot
   * take a `className`.
   */
  it('keeps the Type dropdown out of the canvas drag layer', () => {
    const { getByLabelText } = renderItem({
      variableName: 'task',
      variable: 'task',
      type: 'fixed',
      value: '',
      defaultValue: '',
      onChangeMapping: vi.fn(),
    });

    const escapeHatch = getByLabelText('Type').closest('.nodrag');
    expect(escapeHatch, 'the Type select must sit inside a `nodrag` ancestor').not.toBeNull();
    expect(escapeHatch).toHaveClass('nopan');
  });

  it('keeps the Value dropdown out of the canvas drag layer', () => {
    const { getByLabelText } = renderItem({
      variableName: 'task',
      variable: 'task',
      type: 'variable',
      value: '',
      defaultValue: '',
      onChangeMapping: vi.fn(),
    });

    const escapeHatch = getByLabelText('Value').closest('.nodrag');
    expect(escapeHatch, 'the Value select must sit inside a `nodrag` ancestor').not.toBeNull();
    expect(escapeHatch).toHaveClass('nopan');
  });

  it('JSON.stringifies a non-string value for the resolved display value', () => {
    const { getByLabelText } = renderItem({
      variableName: 'system',
      variable: 'system',
      type: 'fixed',
      value: { foo: 'bar' },
      defaultValue: '',
      onChangeMapping: vi.fn(),
    });
    expect(getByLabelText('Value')).toHaveValue('{"foo":"bar"}');
  });

  it('calls onChangeMapping with the new value on edit', async () => {
    const user = userEvent.setup();
    const onChangeMapping = vi.fn();
    const { getByLabelText } = renderItem({
      variableName: 'system',
      variable: 'system',
      type: 'fixed',
      value: '',
      defaultValue: '',
      onChangeMapping,
    });

    await user.type(getByLabelText('Value'), 'x');

    expect(onChangeMapping).toHaveBeenCalledWith('system', { type: 'fixed', value: 'x' });
  });

  it('parses chat_history JSON input when type is fixed', async () => {
    const user = userEvent.setup();
    const onChangeMapping = vi.fn();
    const { getByLabelText } = renderItem({
      variableName: 'chat_history',
      variable: 'chat_history',
      type: 'fixed',
      value: '',
      defaultValue: '',
      onChangeMapping,
    });

    await user.type(getByLabelText('Value'), '5');

    expect(onChangeMapping).toHaveBeenCalledWith('chat_history', { type: 'fixed', value: 5 });
  });

  it('preserves the value when switching between fstring and fixed types', async () => {
    const user = userEvent.setup();
    const onChangeMapping = vi.fn();
    const { getByLabelText } = renderItem({
      variableName: 'system',
      variable: 'system',
      type: 'fstring',
      value: 'hello',
      defaultValue: 'default',
      onChangeMapping,
    });

    await user.click(getByLabelText('Type'));
    await user.click(document.querySelector('[data-value="fixed"]') as Element);

    expect(onChangeMapping).toHaveBeenCalledWith('system', { type: 'fixed', value: 'hello' });
  });

  it('resets to the default value when switching to a variable type', async () => {
    const user = userEvent.setup();
    const onChangeMapping = vi.fn();
    const { getByLabelText } = renderItem({
      variableName: 'code',
      variable: 'code',
      type: 'fixed',
      value: 'hello',
      defaultValue: 'default',
      onChangeMapping,
    });

    await user.click(getByLabelText('Type'));
    await user.click(document.querySelector('[data-value="variable"]') as Element);

    expect(onChangeMapping).toHaveBeenCalledWith('code', { type: 'variable', value: 'default' });
  });

  it('renders via AIAssistantInput when enableAIAssistant is set for an eligible variable', () => {
    server.use(
      http.get('/api/v2/configurations/available/', () => HttpResponse.json([])),
      http.get('/api/v2/configurations/configurations/7', () => HttpResponse.json({ items: [], total: 0 })),
    );
    configureGeneratedClient({ baseUrl: '/api/v2' });

    const { getByLabelText } = renderItem({
      variableName: 'system',
      variable: 'system',
      type: 'fixed',
      value: 'hi',
      defaultValue: '',
      onChangeMapping: vi.fn(),
      enableAIAssistant: true,
    });

    expect(getByLabelText('AI Assistant')).toBeInTheDocument();
  });

  // Regression coverage: `NodeFieldInput`'s doc comment above records this
  // as a formerly-confirmed cluster finding (#2, A2-settings-panels) — the
  // AI-Assistant-enabled field's visible input used to silently discard
  // direct keystrokes because `../AIAssistantInput.tsx` never wired an
  // `onChange` onto its base `InputBase` (fixed there since). This test
  // exercises the full integration through THIS component, not just
  // `AIAssistantInput.tsx`'s own unit test.
  it('reports edits typed directly into an AI-Assistant-enabled field, not just through the modal', async () => {
    server.use(
      http.get('/api/v2/configurations/available/', () => HttpResponse.json([])),
      http.get('/api/v2/configurations/configurations/7', () => HttpResponse.json({ items: [], total: 0 })),
    );
    configureGeneratedClient({ baseUrl: '/api/v2' });

    const user = userEvent.setup();
    const onChangeMapping = vi.fn();
    const { getByLabelText } = renderItem({
      variableName: 'system',
      variable: 'system',
      type: 'fixed',
      value: 'hi',
      defaultValue: '',
      onChangeMapping,
      enableAIAssistant: true,
    });

    await user.type(getByLabelText('Value'), '!');

    expect(onChangeMapping).toHaveBeenCalledWith('system', { type: 'fixed', value: 'hi!' });
  });

  it('does not enable AI Assistant for an ineligible variable name', () => {
    const { queryByText } = renderItem({
      variableName: 'not_eligible',
      variable: 'not_eligible',
      type: 'fixed',
      value: '',
      defaultValue: '',
      onChangeMapping: vi.fn(),
      enableAIAssistant: true,
    });
    expect(queryByText('AI Assistant')).not.toBeInTheDocument();
  });

  describe('the code field AI-Assistant editor uses the selected language and Python default', () => {
    function stubForModal(): void {
      server.use(
        http.get('/api/v2/configurations/available/', () => HttpResponse.json([])),
        http.get('/api/v2/configurations/configurations/7', () => HttpResponse.json({ items: [], total: 0 })),
      );
      configureGeneratedClient({ baseUrl: '/api/v2' });
    }

    it('opens the AI Assistant editor with "Python" pre-selected for the `code` variable (baseline `language` override)', async () => {
      stubForModal();
      const user = userEvent.setup();

      // 'x = 1' does not match any of `detectContentType`'s python keyword markers
      // (def/class/import/from/print(/if __name__/elif/self.), so without the
      // restored override this would auto-detect as "Text", not "Python" —
      // this is what would have caught the regression.
      const { getByRole } = renderItem({
        variableName: 'code',
        variable: 'code',
        type: 'fixed',
        value: 'x = 1',
        defaultValue: '',
        onChangeMapping: vi.fn(),
        enableAIAssistant: true,
      });

      await user.click(getByRole('button', { name: 'AI Assistant' }));

      expect(getByRole('combobox', { name: 'Content type' })).toHaveTextContent('Python');
    });

    it.each([
      { language: 'python', label: 'Python' },
      { language: 'javascript', label: 'Java script' },
      { language: 'typescript', label: 'Type script' },
      { language: 'rust', label: 'Rust' },
    ])('uses the selected $language Code language in the AI editor', async ({ language, label }) => {
      stubForModal();
      const user = userEvent.setup();
      const { getByRole } = renderItem({
        variableName: 'code', variable: 'code', type: 'fixed', value: 'x = 1', defaultValue: '',
        codeLanguage: language,
        onChangeMapping: vi.fn(), enableAIAssistant: true,
      });
      await user.click(getByRole('button', { name: 'AI Assistant' }));
      expect(getByRole('combobox', { name: 'Content type' })).toHaveTextContent(label);
    });

    it('does not force Python for a non-`code` eligible variable (auto-detected instead)', async () => {
      stubForModal();
      const user = userEvent.setup();

      const { getByRole } = renderItem({
        variableName: 'system',
        variable: 'system',
        type: 'fixed',
        value: 'hi',
        defaultValue: '',
        onChangeMapping: vi.fn(),
        enableAIAssistant: true,
      });

      await user.click(getByRole('button', { name: 'AI Assistant' }));

      expect(getByRole('combobox', { name: 'Content type' })).not.toHaveTextContent('Python');
    });
  });
});
