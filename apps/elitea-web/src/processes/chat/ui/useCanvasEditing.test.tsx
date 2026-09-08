/**
 * The composition point that was a documented no-op.
 *
 * `ChatWithEditors.hooks.ts` shipped `onShowCanvasEditor` as `() => {}` and
 * a `canvasEditorRef` whose `.current` was permanently `null`. That broke
 * two things: opening a canvas did nothing, and `useEditorMutex`'s
 * `closeHandlers.isEditingCanvas` — the save-before-swap branch — resolved
 * to `null?.save?.()` and threw the user's edits away without a word.
 */
import type { ReactElement, ReactNode } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, renderHook, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { useEditorStateStore } from '@/shared/lib/editorState';
import { server } from '@/test/setup';
import { useSelectedProjectStore } from '@/widgets/app-shell';

import { toSelectedCodeBlockInfo, useCanvasEditing } from './useCanvasEditing';

const BASE = '/api/v2';
const PROJECT = '7';

function wrapper({ children }: { children: ReactNode }): ReactElement {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return <QueryClientProvider client={client}>{children}</QueryClientProvider>;
}

/** The saves the route received, in order. */
interface RecordedSave {
  readonly canvasUUID: string;
  readonly body: Record<string, unknown>;
}

function recordSaves(saves: RecordedSave[]): void {
  server.use(
    http.put(`${BASE}/elitea_core/canvas/prompt_lib/${PROJECT}/:canvasUUID`, async ({ params, request }) => {
      saves.push({
        canvasUUID: String(params['canvasUUID']),
        body: (await request.json()) as Record<string, unknown>,
      });
      return HttpResponse.json({ uuid: String(params['canvasUUID']) });
    }),
  );
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
  useSelectedProjectStore.setState({ project: { id: PROJECT, name: 'Project 7' } });
});

afterEach(() => {
  useEditorStateStore.getState().setEditingCanvas(false);
  useSelectedProjectStore.setState({ project: null });
  resetGeneratedClient();
});

describe('toSelectedCodeBlockInfo', () => {
  it('narrows the opaque payload into the editor props', () => {
    const info = toSelectedCodeBlockInfo({
      codeBlock: 'const a = 1;',
      language: 'javascript',
      isBlock: true,
      canvasId: 'cv-1',
      messageItemId: 42,
      viewOnly: true,
    });
    expect(info).toEqual({
      codeBlock: 'const a = 1;',
      language: 'javascript',
      isBlock: true,
      canvasId: 'cv-1',
      messageItemId: 42,
      viewOnly: true,
    });
  });

  it('defaults a missing language rather than passing undefined down', () => {
    expect(toSelectedCodeBlockInfo({ codeBlock: 'x' })?.language).toBe('markdown');
  });

  /*
   * A payload with no code block opens nothing. `CanvasEditor`'s own first
   * guard renders `display: none` for that case, so flipping
   * `isEditingCanvas` would wedge the mutex: every other editor would queue
   * behind a canvas nobody can see or close.
   */
  it('refuses a payload with no code block', () => {
    expect(toSelectedCodeBlockInfo({})).toBeUndefined();
    expect(toSelectedCodeBlockInfo({ codeBlock: '' })).toBeUndefined();
    expect(toSelectedCodeBlockInfo({ codeBlock: 42 })).toBeUndefined();
  });
});

describe('useCanvasEditing', () => {
  it('opens the canvas and raises the shared isEditingCanvas flag', () => {
    const { result } = renderHook(() => useCanvasEditing(), { wrapper });
    expect(result.current.isEditingCanvas).toBe(false);

    act(() => result.current.onShowCanvasEditor({ codeBlock: 'hello', language: 'markdown', isBlock: true }));

    expect(result.current.isEditingCanvas).toBe(true);
    expect(result.current.selectedCodeBlockInfo?.codeBlock).toBe('hello');
    // The flag the editor mutex and the nav blocker both read.
    expect(useEditorStateStore.getState().isAnyEditorOpen).toBe(true);
  });

  it('closes and clears the selection', () => {
    const { result } = renderHook(() => useCanvasEditing(), { wrapper });
    act(() => result.current.onShowCanvasEditor({ codeBlock: 'hello' }));
    act(() => { result.current.onCloseCanvasEditor(); });

    expect(result.current.isEditingCanvas).toBe(false);
    expect(result.current.selectedCodeBlockInfo).toBeUndefined();
  });

  it('does not open for a payload with no code block', () => {
    const { result } = renderHook(() => useCanvasEditing(), { wrapper });
    act(() => result.current.onShowCanvasEditor({ language: 'markdown' }));
    expect(result.current.isEditingCanvas).toBe(false);
  });

  /*
   * The ref is what `useEditorMutex.closeHandlers.isEditingCanvas` calls
   * `.save()` on. It must be a real ref object the caller can attach — the
   * stub returned one whose `.current` nothing ever wrote.
   */
  it('hands back an attachable ref for the mutex to save through', () => {
    const { result } = renderHook(() => useCanvasEditing(), { wrapper });
    expect(result.current.canvasEditorRef).toHaveProperty('current');
    let saved = false;
    result.current.canvasEditorRef.current = { save: () => { saved = true; } };
    result.current.canvasEditorRef.current.save();
    expect(saved).toBe(true);
  });
});

/*
 * THE SAVE. `CanvasEditor` closes with `(hasChange, finalResult, language)` —
 * the live document — and this hook used to take none of the three and make
 * no request at all, so an edit existed only until the drawer closed.
 */
describe('useCanvasEditing — persisting the edit', () => {
  it('PUTs the edited document for the open canvas', async () => {
    const saves: RecordedSave[] = [];
    recordSaves(saves);
    const { result } = renderHook(() => useCanvasEditing(), { wrapper });

    act(() => result.current.onShowCanvasEditor({ codeBlock: 'print(1)', language: 'python', isBlock: true, canvasId: 'cv-9' }));
    act(() => { result.current.onCloseCanvasEditor(true, 'print(2)', 'python'); });

    await waitFor(() => expect(saves).toHaveLength(1));
    expect(saves[0]?.canvasUUID).toBe('cv-9');
    expect(saves[0]?.body['canvas_content']).toBe('print(2)');
    expect(saves[0]?.body['code_language']).toBe('python');
    // The editor still closes, whatever the request does.
    expect(result.current.isEditingCanvas).toBe(false);
  });

  it('saves a language switch even when the text is untouched', async () => {
    const saves: RecordedSave[] = [];
    recordSaves(saves);
    const { result } = renderHook(() => useCanvasEditing(), { wrapper });

    act(() => result.current.onShowCanvasEditor({ codeBlock: 'x = 1', language: 'python', isBlock: true, canvasId: 'cv-9' }));
    act(() => { result.current.onCloseCanvasEditor(false, 'x = 1', 'javascript'); });

    await waitFor(() => expect(saves).toHaveLength(1));
    expect(saves[0]?.body['code_language']).toBe('javascript');
  });

  /*
   * Three closes that must write NOTHING. Each one is a request that would
   * either 404 or overwrite a canvas with something the user never typed.
   */
  it('writes nothing when the block has no canvas of its own', async () => {
    const saves: RecordedSave[] = [];
    recordSaves(saves);
    const { result } = renderHook(() => useCanvasEditing(), { wrapper });

    act(() => result.current.onShowCanvasEditor({ codeBlock: 'x = 1', language: 'python', isBlock: true }));
    act(() => { result.current.onCloseCanvasEditor(true, 'x = 2', 'python'); });

    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(saves).toEqual([]);
  });

  it('writes nothing when the editor reports no change', async () => {
    const saves: RecordedSave[] = [];
    recordSaves(saves);
    const { result } = renderHook(() => useCanvasEditing(), { wrapper });

    act(() => result.current.onShowCanvasEditor({ codeBlock: 'x = 1', language: 'python', isBlock: true, canvasId: 'cv-9' }));
    act(() => { result.current.onCloseCanvasEditor(false, 'x = 1', 'python'); });

    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(saves).toEqual([]);
  });

  /*
   * A close with no arguments — the drawer's backdrop, the nav blocker — is
   * not an edit. This is also the guard against the MUI `onClose(event,
   * reason)` shape reaching the save as `(hasChange, finalResult)`, which
   * would store the string "backdropClick" as the user's document.
   */
  it('writes nothing when closed with no document', async () => {
    const saves: RecordedSave[] = [];
    recordSaves(saves);
    const { result } = renderHook(() => useCanvasEditing(), { wrapper });

    act(() => result.current.onShowCanvasEditor({ codeBlock: 'x = 1', language: 'python', isBlock: true, canvasId: 'cv-9' }));
    act(() => { result.current.onCloseCanvasEditor(); });

    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(saves).toEqual([]);
  });

  it('writes nothing for a canvas opened read-only', async () => {
    const saves: RecordedSave[] = [];
    recordSaves(saves);
    const { result } = renderHook(() => useCanvasEditing(), { wrapper });

    act(() =>
      result.current.onShowCanvasEditor({ codeBlock: 'x = 1', language: 'python', isBlock: true, canvasId: 'cv-9', viewOnly: true }),
    );
    act(() => { result.current.onCloseCanvasEditor(true, 'x = 2', 'python'); });

    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(saves).toEqual([]);
  });
});
