import { ThemeProvider } from '@mui/material/styles';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { EditorView } from '@codemirror/view';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';
import * as runtimeConfig from '@/shared/config';
import { server } from '@/test/setup';

const BASE = 'http://elitea.test/api/v2';
const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

function show(ui: React.ReactElement) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <ThemeProvider theme={theme} defaultMode={DEFAULT_COLOR_SCHEME}>{ui}</ThemeProvider>
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  window.localStorage.clear();
  configureGeneratedClient({ baseUrl: BASE });
  // The bucket list is fetched through the artifacts feature's own hook,
  // which reads the runtime config for its base URL. Without this the query
  // fails and the picker offers no bucket — a green test over a dead control.
  vi.spyOn(runtimeConfig, 'getConfig').mockReturnValue({
    status: 'ok',
    config: {
      vite_server_url: BASE,
      vite_base_uri: '/',
      vite_public_project_id: 'public',
      allow_project_own_llms: false,
    },
  });
  server.use(
    http.get(`${BASE}/artifacts/buckets/:projectId`, () =>
      HttpResponse.json({
        buckets: [
          { name: 'docs', is_pinned: false, created_at: '2026-01-01T00:00:00Z' },
          { name: 'handbooks', is_pinned: false, created_at: '2026-01-01T00:00:00Z' },
        ],
      }),
    ),
  );
});
afterEach(() => {
  resetGeneratedClient();
  vi.restoreAllMocks();
});

import { WikiSettingsPanel } from './WikiSettingsPanel';
import { installCodeMirrorTestPolyfills } from '@/shared/ui/lib/field/codeMirrorTestPolyfills';

// CodeMirror measures DOM ranges on every edit; jsdom has no layout, so the
// same polyfills the editor's own suite installs are needed to type into it.
installCodeMirrorTestPolyfills();

const TOOLKIT = { id: 42, name: 'Wikis', type: 'wikis', description: 'kept' };

async function replaceDraft(_user: ReturnType<typeof userEvent.setup>, text: string) {
  const content = document.querySelector('.cm-content');
  if (!(content instanceof HTMLElement)) throw new Error('no editor');
  // Through the editor's own API, as one transaction. Typing `{` key by key
  // ran CodeMirror's bracket auto-closing into the keystrokes, and a
  // user-event paste reached the CI shard's jsdom with no clipboard data at
  // all — the draft it saved was "" (the CI run received data-field="").
  // The panel reads the draft through CodeMirrorEditor's debounced onChange
  // (30ms), so the dispatch is followed by a wait longer than that.
  const view = EditorView.findFromDOM(content);
  if (view === null) throw new Error('no EditorView behind .cm-content');
  await act(async () => {
    view.dispatch({ changes: { from: 0, to: view.state.doc.length, insert: text } });
    await new Promise((resolve) => setTimeout(resolve, 80));
  });
  expect(content).toHaveTextContent(text.slice(0, 12));
}

describe('WikiSettingsPanel', () => {
  it('a Save pressed before the debounced draft catches up still judges what the editor HOLDS', async () => {
    let saved = 0;
    server.use(
      http.put(`${BASE}/elitea_core/tool/prompt_lib/:projectId/:toolkitId`, () => {
        saved += 1;
        return HttpResponse.json({ ...TOOLKIT, settings: {} });
      }),
    );
    show(<WikiSettingsPanel projectId="7" toolkitId="42" toolkit={TOOLKIT} settings={{ repository: 'acme/svc' }} />);
    const content = document.querySelector('.cm-content');
    if (!(content instanceof HTMLElement)) throw new Error('no editor');
    const view = EditorView.findFromDOM(content);
    if (view === null) throw new Error('no EditorView');
    // Replace the document and press Save in the same tick: `draft` still
    // holds the valid seeded settings, the editor holds a draft with no
    // repository. The journey on the real stack saved the OLD settings here.
    // Synchronous on purpose: user-event's click has its own awaits, long
    // enough for the 30ms debounce to deliver the draft first and hide the race.
    act(() => {
      view.dispatch({ changes: { from: 0, to: view.state.doc.length, insert: '{"llm_model": "gpt-5"}' } });
      fireEvent.click(screen.getByTestId('wiki-settings-save'));
    });
    const problem = await screen.findByTestId('wiki-settings-problem');
    expect(problem).toHaveAttribute('data-field', 'repository');
    expect(saved).toBe(0);
    expect(screen.queryByTestId('wiki-settings-saved')).toBeNull();
  });

  it('refuses a draft that is not JSON, against the document', async () => {
    const user = userEvent.setup();
    show(<WikiSettingsPanel projectId="7" toolkitId="42" toolkit={TOOLKIT} settings={{ repository: 'acme/svc' }} />);
    await replaceDraft(user, '{not json');
    const problem = await screen.findByTestId('wiki-settings-problem');
    expect(problem).toHaveAttribute('data-field', '');
    expect(screen.getByTestId('wiki-settings-save')).toBeDisabled();
  });

  it('refuses a configuration with no repository, against the field', async () => {
    // The one check the legacy screen never made; without it a generation
    // runs and finds nothing.
    const user = userEvent.setup();
    show(<WikiSettingsPanel projectId="7" toolkitId="42" toolkit={TOOLKIT} settings={{ repository: 'acme/svc' }} />);
    await replaceDraft(user, '{"llm_model": "gpt-5"}');
    const problem = await screen.findByTestId('wiki-settings-problem');
    expect(problem).toHaveAttribute('data-field', 'repository');
    expect(screen.getByTestId('wiki-settings-save')).toBeDisabled();
  });

  it('PUTs the WHOLE toolkit with the new settings', async () => {
    // The route replaces the resource; a settings-only body would clear every
    // other field the toolkit carries.
    const user = userEvent.setup();
    let body: Record<string, unknown> | null = null;
    server.use(
      http.put(`${BASE}/elitea_core/tool/prompt_lib/:projectId/:toolkitId`, async ({ request }) => {
        body = (await request.json()) as Record<string, unknown>;
        return HttpResponse.json({ ...body, id: 42 });
      }),
    );
    show(<WikiSettingsPanel projectId="7" toolkitId="42" toolkit={TOOLKIT} settings={{ repository: 'acme/svc' }} />);
    await user.click(screen.getByTestId('wiki-settings-save'));
    await screen.findByTestId('wiki-settings-saved');
    expect(body).toMatchObject({ id: 42, name: 'Wikis', description: 'kept', settings: { repository: 'acme/svc' } });
  });

  it('reports a failed save and leaves the draft as typed', async () => {
    const user = userEvent.setup();
    server.use(
      http.put(`${BASE}/elitea_core/tool/prompt_lib/:projectId/:toolkitId`, () =>
        HttpResponse.json({ error: 'nope' }, { status: 500 }),
      ),
    );
    show(<WikiSettingsPanel projectId="7" toolkitId="42" toolkit={TOOLKIT} settings={{ repository: 'acme/svc' }} />);
    await user.click(screen.getByTestId('wiki-settings-save'));
    expect(await screen.findByTestId('wiki-settings-error')).toBeVisible();
    expect(document.querySelector('.cm-content')?.textContent).toContain('acme/svc');
  });

  it('names both model settings when the toolkit configures neither, WITHOUT blocking Save', async () => {
    // Measured 2026-09-02 (PR #725): the engine asks the gateway for
    // gpt-4o-mini / text-embedding-3-large when the toolkit names neither, the
    // gateway resolves models per project, and a project without those rows
    // answers 404 — the generation "completes" with no pages and only the
    // gateway log says why. The document is still legal, so Save stays live.
    const user = userEvent.setup();
    show(<WikiSettingsPanel projectId="7" toolkitId="42" toolkit={TOOLKIT} settings={{ repository: 'acme/svc' }} />);
    await replaceDraft(user, '{"repository": "acme/svc"}');
    const hints = await screen.findAllByTestId('wiki-settings-hint');
    expect(hints.map((hint) => hint.getAttribute('data-field'))).toEqual(['llm_model', 'embedding_model']);
    // The model the engine would ask for is named, because that is the string
    // the operator has to configure or match.
    expect(hints[0]).toHaveTextContent('gpt-4o-mini');
    expect(hints[1]).toHaveTextContent('text-embedding-3-large');
    // No literal braces: a `{{fallback}}` that never interpolated would render
    // as itself and still satisfy a looser assertion.
    expect(hints[1]?.textContent).not.toContain('{{');
    expect(screen.getByTestId('wiki-settings-save')).toBeEnabled();
    expect(screen.queryByTestId('wiki-settings-problem')).toBeNull();
  });

  it('drops the hint for a model the toolkit DOES name', async () => {
    const user = userEvent.setup();
    show(<WikiSettingsPanel projectId="7" toolkitId="42" toolkit={TOOLKIT} settings={{ repository: 'acme/svc' }} />);
    await replaceDraft(user, '{"repository": "acme/svc", "llm_model": "gpt-5"}');
    const hints = await screen.findAllByTestId('wiki-settings-hint');
    expect(hints.map((hint) => hint.getAttribute('data-field'))).toEqual(['embedding_model']);
  });

  describe('the source picker', () => {
    // The picker writes into the SAME draft the JSON editor holds, so the
    // Save button, the validator and the saved document are the ones a
    // hand-typed `artifact_configuration` already goes through.
    function editorText(): string {
      return document.querySelector('.cm-content')?.textContent ?? '';
    }

    async function chooseBucket(user: ReturnType<typeof userEvent.setup>, name: string) {
      await user.click(screen.getByTestId('wiki-source-kind-folder'));
      const field = await screen.findByTestId('wiki-source-bucket');
      await user.click(within(field).getByRole('combobox'));
      await user.click(await screen.findByRole('option', { name }));
    }

    it('offers the project\'s own buckets', async () => {
      const user = userEvent.setup();
      show(<WikiSettingsPanel projectId="7" toolkitId="42" toolkit={TOOLKIT} settings={{ repository: 'acme/svc' }} />);
      await user.click(screen.getByTestId('wiki-source-kind-folder'));
      const field = await screen.findByTestId('wiki-source-bucket');
      await user.click(within(field).getByRole('combobox'));
      await waitFor(() => {
        expect(screen.getAllByRole('option').map((option) => option.textContent)).toEqual(['docs', 'handbooks']);
      });
    });

    it('writes the folder into the draft and removes the repository', async () => {
      // Both sources cannot stand: the facade refuses the pair. A picker that
      // added the folder and left the repository would produce exactly the
      // document the validator then refuses.
      const user = userEvent.setup();
      let body: Record<string, unknown> | null = null;
      server.use(
        http.put(`${BASE}/elitea_core/tool/prompt_lib/:projectId/:toolkitId`, async ({ request }) => {
          body = (await request.json()) as Record<string, unknown>;
          return HttpResponse.json({ ...body, id: 42 });
        }),
      );
      show(<WikiSettingsPanel projectId="7" toolkitId="42" toolkit={TOOLKIT} settings={{ github_repository: 'acme/svc', code_toolkit: 9 }} />);
      await chooseBucket(user, 'docs');
      await user.type(screen.getByTestId('wiki-source-prefix'), 'handbook');
      await waitFor(() => {
        expect(editorText()).toContain('handbook');
      });
      expect(editorText()).not.toContain('github_repository');
      expect(editorText()).not.toContain('code_toolkit');
      expect(screen.queryByTestId('wiki-settings-problem')).toBeNull();

      await user.click(screen.getByTestId('wiki-settings-save'));
      // The request first, the notice second. Eight keystrokes rewrote the
      // CodeMirror document eight times, and waiting only for the notice put
      // the whole of that work inside one findBy timeout.
      await waitFor(() => { expect(body).not.toBeNull(); });
      await screen.findByTestId('wiki-settings-saved');
      // AND THE NOTICE STAYS. CodeMirror echoes a programmatic write back
      // through its debounced onChange 30ms later; read as a fresh edit, that
      // echo cleared the notice a moment after the save it belongs to.
      await act(async () => { await new Promise((settle) => setTimeout(settle, 120)); });
      expect(screen.getByTestId('wiki-settings-saved')).toBeVisible();
      expect(body).toMatchObject({
        settings: { artifact_configuration: { bucket: 'docs', prefix: 'handbook' } },
      });
      expect((body as unknown as { settings: Record<string, unknown> }).settings).not.toHaveProperty('code_toolkit');
    });

    it('writes nothing until a bucket is chosen', async () => {
      // Half a choice is not a source, and it must not destroy one either:
      // the configured repository stays until a bucket replaces it, so an
      // operator who opens the folder side and changes their mind still has
      // the toolkit they came in with.
      const user = userEvent.setup();
      show(<WikiSettingsPanel projectId="7" toolkitId="42" toolkit={TOOLKIT} settings={{ repository: 'acme/svc' }} />);
      await user.click(screen.getByTestId('wiki-source-kind-folder'));
      await screen.findByTestId('wiki-source-bucket');
      expect(editorText()).not.toContain('artifact_configuration');
      expect(editorText()).toContain('acme/svc');
      expect(screen.queryByTestId('wiki-settings-problem')).toBeNull();
      expect(screen.getByTestId('wiki-settings-save')).toBeEnabled();
    });

    it('choosing Repository takes the folder back out of the draft', async () => {
      const user = userEvent.setup();
      show(
        <WikiSettingsPanel
          projectId="7"
          toolkitId="42"
          toolkit={TOOLKIT}
          settings={{ artifact_configuration: { bucket: 'docs', prefix: 'handbook' } }}
        />,
      );
      expect(editorText()).toContain('artifact_configuration');
      await user.click(screen.getByTestId('wiki-source-kind-repository'));
      await waitFor(() => {
        expect(editorText()).not.toContain('artifact_configuration');
      });
      // And the document is then a document with no source, which is refused.
      const problem = await screen.findByTestId('wiki-settings-problem');
      expect(problem).toHaveAttribute('data-field', 'repository');
    });

    it('opens on the folder side for a toolkit already configured with one', () => {
      show(
        <WikiSettingsPanel
          projectId="7"
          toolkitId="42"
          toolkit={TOOLKIT}
          settings={{ artifact_configuration: { bucket: 'docs', prefix: 'handbook' } }}
        />,
      );
      expect(screen.getByTestId('wiki-source-kind-folder')).toHaveAttribute('aria-pressed', 'true');
      expect(screen.getByTestId('wiki-source-prefix')).toHaveValue('handbook');
    });

    it('cannot rewrite a document it cannot read', async () => {
      // The picker rewrites the JSON. While the draft does not parse there is
      // nothing to rewrite, and a control that silently replaced the text with
      // `{}` would discard whatever the operator was in the middle of typing.
      const user = userEvent.setup();
      show(<WikiSettingsPanel projectId="7" toolkitId="42" toolkit={TOOLKIT} settings={{ repository: 'acme/svc' }} />);
      await replaceDraft(user, '{not json');
      await screen.findByTestId('wiki-settings-problem');
      expect(screen.getByTestId('wiki-source-kind-folder')).toBeDisabled();
      expect(editorText()).toContain('{not json');
    });
  });

  it('says nothing about models while the draft is not JSON', async () => {
    // A hint beside "not valid JSON" would read as a second, unrelated defect.
    const user = userEvent.setup();
    show(<WikiSettingsPanel projectId="7" toolkitId="42" toolkit={TOOLKIT} settings={{ repository: 'acme/svc' }} />);
    await replaceDraft(user, '{not json');
    await screen.findByTestId('wiki-settings-problem');
    expect(screen.queryAllByTestId('wiki-settings-hint')).toEqual([]);
  });
});
