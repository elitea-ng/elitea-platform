import type { ComponentProps } from 'react';
import { createRef } from 'react';

import { ThemeProvider } from '@mui/material/styles';
import { EditorView } from '@codemirror/view';
import { RouterProvider, createMemoryHistory, createRootRoute, createRouter } from '@tanstack/react-router';
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';
import { forceResizeObserverAbsentForTest } from '@/shared/ui/lib/field/codeMirrorTestPolyfills';

import { usePipelineYamlStore } from '../model/pipelineYamlStore';
import { load } from 'js-yaml';
import { dumpYaml } from '../lib/dumpYaml.helpers';
import { EditorPanel } from './EditorPanel';
import type { EditorPanelHandle } from './EditorPanel';

/**
 * A local, narrower stand-in for `shared/ui/lib/field/codeMirrorTestPolyfills`'s
 * `installCodeMirrorTestPolyfills()`: only the `Range` measurement stubs the
 * Yaml-mode `YamlCodeEditor` (CodeMirror 6) needs to mount without an
 * unhandled `getClientRects is not a function` rejection. Deliberately NOT
 * the full helper — that one ALSO defines `window.ResizeObserver` when
 * missing, which this file's own "shows the error-boundary fallback"/
 * "reload button" tests below depend on staying UNDEFINED (verified
 * directly: installing it made both fail — `FlowWrapper`'s real, currently
 * broken chain crashes specifically because `useFlowEditorResizeObserver`
 * calls a global `ResizeObserver` that does not exist in this jsdom
 * environment; silently providing one changes that real, disclosed gap's
 * observable behaviour out from under those tests).
 */
function installRangeMeasurementPolyfills(): void {
  if (typeof Range.prototype.getClientRects !== 'function') {
    Range.prototype.getClientRects = () => ({ length: 0, item: () => null, [Symbol.iterator]: () => [][Symbol.iterator]() });
  }
  if (typeof Range.prototype.getBoundingClientRect !== 'function') {
    Range.prototype.getBoundingClientRect = () => ({ bottom: 0, height: 0, left: 0, right: 0, top: 0, width: 0, x: 0, y: 0, toJSON: () => ({}) });
  }
}
installRangeMeasurementPolyfills();

/**
 * This file's "shows the error-boundary fallback"/"reload button"/
 * "onRcvAgentEvent…"/"accepts versionTools/llmSettings…" tests all depend on
 * `window.ResizeObserver` being unavailable (see the module doc comment
 * above and `codeMirrorTestPolyfills.ts`'s own doc comment on
 * `forceResizeObserverAbsentForTest`). Other test files in the same vitest
 * worker permanently define it via `installCodeMirrorTestPolyfills()`, so
 * this is required for every test in this file, not just the four that read
 * it directly — reproduced as a real, order-dependent flake, not a
 * hypothetical one.
 */
forceResizeObserverAbsentForTest();

/**
 * No `vi.mock()` here (`no-vi-mock`/R-M1 forbids it outright, verified via
 * `npx oxlint` — this codebase substitutes only the network boundary/MSW
 * and the socket double, never an application module). `EditorPanel`
 * `React.lazy()`-loads `./FlowWrapper` (which statically imports
 * `./FlowEditor`, unit A2k), and that chain has a real, verified,
 * currently-broken transitive dependency (`ui/state/RunStateDialog.status.
 * tsx` imports a non-existent `@mui/icons-material/ErrorOutline` — see
 * `flowWrapperStyles.ts`'s own doc comment for the full citation). Every
 * test below renders the REAL, unmocked component tree; verified directly
 * (before writing these assertions) that `React.lazy`'s deferred-resolution
 * + this file's own `FlowEditorErrorBoundary` together catch that failure
 * gracefully at test time exactly like they would in production — the
 * "shows the error-boundary fallback" test below asserts that REAL,
 * currently-true behaviour, not a workaround. `AddNodeMenu`/the mode toggle/
 * the copy button are direct JSX SIBLINGS of the lazy-loaded flow pane, not
 * descendants, so they render and are fully testable independent of whether
 * that pane itself ever resolves.
 */
const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

beforeEach(() => {
  usePipelineYamlStore.setState({
    yamlCode: 'nodes: []',
    yamlJsonObject: { nodes: [] },
    initYamlCode: 'nodes: []',
    initYamlJsonObject: { nodes: [] },
    resetFlag: false,
    layoutVersion: undefined,
    stateKeyOrder: [],
    initStateKeyOrder: [],
  });
});

afterEach(() => {
  vi.clearAllMocks();
});

function renderEditorPanel(
  props: Partial<ComponentProps<typeof EditorPanel>> = {},
  path = '/pipelines/latest/1',
  ref?: React.Ref<EditorPanelHandle>,
) {
  const rootRoute = createRootRoute({
    component: () => (
      <ThemeProvider
        theme={theme}
        defaultMode={DEFAULT_COLOR_SCHEME}
      >
        <EditorPanel
          setYamlDirty={vi.fn()}
          stopRun={vi.fn()}
          ref={ref}
          {...props}
        />
      </ThemeProvider>
    ),
  });
  const router = createRouter({ routeTree: rootRoute, history: createMemoryHistory({ initialEntries: [path] }) });
  return render(<RouterProvider router={router} />);
}

describe('EditorPanel', () => {
  it('starts in Flow mode: shows the AddNodeMenu trigger, not the copy button', async () => {
    renderEditorPanel();
    expect(await screen.findByRole('button', { name: 'Add node' })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /copy/i })).not.toBeInTheDocument();
  });

  it('switching to Yaml mode hides the AddNodeMenu and shows the copy button', async () => {
    const user = userEvent.setup();
    renderEditorPanel();

    await user.click(await screen.findByRole('button', { name: 'Yaml' }));

    expect(screen.queryByRole('button', { name: 'Add node' })).not.toBeInTheDocument();
    await waitFor(() => expect(screen.getByRole('button', { name: /copy yaml code/i })).toBeInTheDocument());
  });

  it('calls setYamlDirty with the live dirty state', async () => {
    const setYamlDirty = vi.fn();
    renderEditorPanel({ setYamlDirty });
    await waitFor(() => expect(setYamlDirty).toHaveBeenCalledWith(false));
  });

  it('keeps a plain load of a legacy decision pipeline clean and preserves stored YAML', async () => {
    const legacy = {
      entry_point: 'Decision_1',
      nodes: [{ id: 'Decision_1', decision: { nodes: ['LLM_1'] } }, { id: 'LLM_1', type: 'llm', transition: 'END' }],
    };
    const stored = dumpYaml(legacy);
    usePipelineYamlStore.setState({
      yamlCode: stored,
      yamlJsonObject: legacy,
      initYamlCode: stored,
      initYamlJsonObject: legacy,
    });
    const setYamlDirty = vi.fn();

    renderEditorPanel({ setYamlDirty });

    await waitFor(() => expect(setYamlDirty).toHaveBeenCalledWith(false));
    expect(setYamlDirty).not.toHaveBeenCalledWith(true);
    expect(usePipelineYamlStore.getState().yamlCode).toBe(stored);
  });

  it('shows the error-boundary fallback for the flow pane, reflecting the real current state of the sibling FlowEditor dependency chain', async () => {
    renderEditorPanel();
    // Generous wait window, same reason `PipelineEditor.test.tsx` documents:
    // the first worker file to trigger the lazy `./FlowWrapper` import pays
    // the transform cost for the whole chain before it can reject. The
    // assertion is unchanged — only the time it is given.
    expect(await screen.findByText('Failed to load the flow editor', {}, { timeout: 20000 })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Reload page' })).toBeInTheDocument();
  }, 30000);

  it('the error-boundary reload button calls window.location.reload', async () => {
    const reloadSpy = vi.fn();
    vi.stubGlobal('location', { reload: reloadSpy });

    const user = userEvent.setup();
    renderEditorPanel();
    await user.click(await screen.findByRole('button', { name: 'Reload page' }, { timeout: 5000 }));
    expect(reloadSpy).toHaveBeenCalledTimes(1);

    vi.unstubAllGlobals();
  }, 10000);

  it('exposes onRcvAgentEvent/deleteAllRunNodes/fitView/onStopRun/hasRunsInProgress on the imperative ref, and none of them throw even though the flow pane failed to load', async () => {
    const ref = createRef<EditorPanelHandle>();
    renderEditorPanel({}, '/pipelines/latest/1', ref);

    await waitFor(() => expect(ref.current).not.toBeNull());
    expect(() => ref.current?.onRcvAgentEvent({})).not.toThrow();
    expect(() => ref.current?.deleteAllRunNodes()).not.toThrow();
    expect(() => ref.current?.fitView()).not.toThrow();
    expect(() => ref.current?.onStopRun()).not.toThrow();
    expect(ref.current?.hasRunsInProgress()).toBe(false);
  });

  it('disabled=true is forwarded to AddNodeMenu (disables its trigger button)', async () => {
    renderEditorPanel({ disabled: true });
    expect(await screen.findByRole('button', { name: 'Add node' })).toBeDisabled();
  });

  it('accepts versionTools/llmSettings without crashing (still shows the error-boundary fallback for the flow pane — see this file\'s own module doc comment: the lazy FlowWrapper/FlowEditor chain has a real, unrelated broken transitive dependency, so a value-level assertion that these two reach FlowEditor itself is not possible from this test)', async () => {
    renderEditorPanel({
      versionTools: [{ id: '1', type: 'toolkit', name: 'search' }],
      llmSettings: { model_name: 'gpt-4o', temperature: 0.7, max_tokens: 1024 },
    });
    expect(await screen.findByRole('button', { name: 'Add node' })).toBeInTheDocument();
    expect(await screen.findByText('Failed to load the flow editor', {}, { timeout: 5000 })).toBeInTheDocument();
  }, 10000);

  it('typing in the YAML editor commits the raw code to the store (onChangeCode)', async () => {
    const user = userEvent.setup();
    renderEditorPanel();

    await user.click(await screen.findByRole('button', { name: 'Yaml' }));
    const editor = await screen.findByRole('textbox');
    await user.type(editor, 'x');

    await waitFor(() => expect(usePipelineYamlStore.getState().yamlCode).toContain('x'));
  });

  it('switching Flow -> Yaml -> Flow parses without rewriting source, updating yamlJsonObject from the (possibly edited) yamlCode', async () => {
    const user = userEvent.setup();
    usePipelineYamlStore.setState({
      yamlCode: 'nodes:\n  - id: entry_point\n    type: entry_point\n',
      yamlJsonObject: { nodes: [{ id: 'entry_point', type: 'entry_point' }] },
      initYamlCode: '',
      initYamlJsonObject: {},
      resetFlag: false,
      layoutVersion: undefined,
    });
    renderEditorPanel();

    await user.click(await screen.findByRole('button', { name: 'Yaml' }));
    await screen.findByRole('textbox');
    await user.click(await screen.findByRole('button', { name: 'Flow' }));

    expect(usePipelineYamlStore.getState().yamlJsonObject).toEqual({
      nodes: [{ id: 'entry_point', type: 'entry_point' }],
    });
  });

  it.each([
    {
      kind: 'valid',
      source: '# retain draft\nname: revised\nstate:\n  first: {type: string, value: null}\nx_future: null\nnodes: []\n',
      parsed: { name: 'revised', state: { first: { type: 'string', value: null } }, x_future: null, nodes: [] },
    },
    { kind: 'invalid', source: 'state: [\nnodes: []\n', parsed: { nodes: [] } },
  ])('retains a $kind draft when switching to Flow before the editor debounce fires', async ({ source, parsed }) => {
    const user = userEvent.setup();
    const { unmount } = renderEditorPanel();
    await user.click(await screen.findByRole('button', { name: 'Yaml' }));
    const editor = await screen.findByRole('textbox');
    const view = EditorView.findFromDOM(editor);
    if (!view) throw new Error('Live YAML CodeMirror view not found');
    const flowButton = screen.getByRole('button', { name: 'Flow' });

    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    try {
      act(() => view.dispatch({ changes: { from: 0, to: view.state.doc.length, insert: source } }));
      act(() => {
        vi.advanceTimersByTime(29);
      });
      expect(view.state.doc.toString()).toBe(source);
      expect(usePipelineYamlStore.getState().yamlCode).toBe('nodes: []');

      fireEvent.click(flowButton);
      expect(screen.queryByRole('textbox')).not.toBeInTheDocument();
      expect(usePipelineYamlStore.getState().yamlCode).toBe(source);
      expect(usePipelineYamlStore.getState().yamlJsonObject).toEqual(parsed);

      act(() => {
        vi.advanceTimersByTime(1);
      });
      expect(usePipelineYamlStore.getState().yamlCode).toBe(source);
      fireEvent.click(screen.getByRole('button', { name: 'Yaml' }));
      expect(EditorView.findFromDOM(screen.getByRole('textbox'))?.state.doc.toString()).toBe(source);
    } finally {
      unmount();
      vi.useRealTimers();
    }
  });

  it('clicking the already-active mode tab is a no-op (mode === newMode early return)', async () => {
    const user = userEvent.setup();
    renderEditorPanel();

    await user.click(await screen.findByRole('button', { name: 'Flow' }));
    // Still in Flow mode — the AddNodeMenu trigger is still the one shown.
    expect(await screen.findByRole('button', { name: 'Add node' })).toBeInTheDocument();
  });

  it('renders with the chat-path layout (isFromChat branches, including the Yaml-mode container styling) without crashing', async () => {
    const user = userEvent.setup();
    renderEditorPanel({}, '/chat/latest/1');
    expect(await screen.findByRole('button', { name: 'Add node' })).toBeInTheDocument();

    await user.click(await screen.findByRole('button', { name: 'Yaml' }));
    expect(await screen.findByRole('textbox')).toBeInTheDocument();
  });

  it('renders the isSmallWindow=false branch (no minHeight override on the Yaml editor container) on a wide window', async () => {
    // jsdom's own default `window.innerWidth` (1024) is already below
    // `MIN_LARGE_WINDOW_WIDTH` (1200), so every OTHER Yaml-mode test in this file already
    // exercises the isSmallWindow=true branch without asking for it explicitly — this is
    // the one test that needs a genuinely wide window to reach the opposite branch.
    Object.defineProperty(window, 'innerWidth', { configurable: true, writable: true, value: 1440 });
    const user = userEvent.setup();
    try {
      renderEditorPanel();
      await user.click(await screen.findByRole('button', { name: 'Yaml' }));
      expect(await screen.findByRole('textbox')).toBeInTheDocument();
    } finally {
      Object.defineProperty(window, 'innerWidth', { configurable: true, writable: true, value: 1024 });
    }
  });

  it('leaves legacy decision source and parsed view intact on mount', async () => {
    usePipelineYamlStore.setState({
      yamlCode: 'nodes:\n  - id: entry_point\n    type: entry_point\n    decision:\n      x: "1"\n',
      yamlJsonObject: { nodes: [{ id: 'entry_point', type: 'entry_point', decision: { x: '1' } }] },
      initYamlCode: '',
      initYamlJsonObject: {},
      resetFlag: false,
      layoutVersion: undefined,
    });
    renderEditorPanel();

    // Viewing a legacy decision keeps the source shape intact.
    await waitFor(() => expect(usePipelineYamlStore.getState().yamlJsonObject).toMatchObject({
      nodes: [expect.objectContaining({ id: 'entry_point', decision: { x: '1' } })],
    }));
  });
  it('preserves exact commented CRLF YAML through Flow/Yaml tabs and a layout-only save', async () => {
    const source =
      '# keep bytes\r\nstate:\r\n  "2": list\r\n  "10": {type: dict, value: null, future: true}\r\nnodes: []\r\n';
    usePipelineYamlStore
      .getState()
      .initPipelineYaml({ yamlCode: source, yamlJsonObject: load(source) as Record<string, unknown> });
    const user = userEvent.setup();
    renderEditorPanel();
    await user.click(await screen.findByRole('button', { name: 'Yaml' }));
    await user.click(await screen.findByRole('button', { name: 'Flow' }));
    usePipelineYamlStore.getState().setLayoutVersion('new-layout');
    usePipelineYamlStore.getState().markYamlCodeSaved();
    expect(usePipelineYamlStore.getState().yamlCode).toBe(source);
    expect(usePipelineYamlStore.getState().initYamlCode).toBe(source);
    expect(usePipelineYamlStore.getState().stateKeyOrder).toEqual(['2', '10']);
  });
});
