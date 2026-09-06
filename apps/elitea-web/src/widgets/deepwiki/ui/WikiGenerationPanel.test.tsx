import { ThemeProvider } from '@mui/material/styles';
import { QueryClient, QueryClientProvider, useQuery } from '@tanstack/react-query';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';
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
});
afterEach(() => {
  resetGeneratedClient();
});

import { createGenerationStorage } from '../lib/generationStorage';
import { WikiGenerationPanel } from './WikiGenerationPanel';

const SETTINGS = { repository: 'acme/svc', code_toolkit: 9, toolkit_configuration_llm_model: 'gpt-5' };

function serveSlots(canStart = true) {
  server.use(
    http.get(`${BASE}/deepwiki/slots/:projectId`, () =>
      HttpResponse.json({ available: 2, total: 3, active: 1, can_start: canStart, mode: 'subprocess' }),
    ),
  );
}

function servePolls(polls: unknown[]) {
  const queue = [...polls];
  const polled: string[] = [];
  server.use(
    http.get(`${BASE}/deepwiki/invocations/:projectId/:toolkit/:tool/:invocation`, ({ params }) => {
      polled.push(String(params['invocation']));
      return HttpResponse.json(queue.shift() ?? { status: 'InProgress' });
    }),
  );
  return polled;
}


/**
 * A query under the `deepwiki` root that counts its own fetches. Completion
 * must invalidate the whole root (the toolkit, its pages, the page list) —
 * this is how the test sees that happen, or not.
 */
let probeFetches = 0;
function DeepWikiProbe(): null {
  useQuery({
    queryKey: ['deepwiki', 'probe'],
    queryFn: () => {
      probeFetches += 1;
      return probeFetches;
    },
  });
  return null;
}

describe('WikiGenerationPanel', () => {
  it('refuses to start when the settings name no source at all, and sends nothing', async () => {
    const user = userEvent.setup();
    serveSlots();
    let posted = false;
    server.use(http.post(`${BASE}/deepwiki/tools/:p/:tk/:tool/invoke`, () => { posted = true; return HttpResponse.json({ invocation_id: 'x' }); }));
    show(<WikiGenerationPanel projectId="7" toolkitId="42" settings={{ repository: 'acme/svc' }} hasWiki={false} />);
    await user.click(await screen.findByTestId('wiki-generate'));
    // The message names BOTH sources: a folder is as good an answer as a
    // repository toolkit, and one that named only `code_toolkit` sent an
    // operator who had chosen a folder looking for the wrong setting.
    expect(await screen.findByTestId('wiki-generate-error')).toHaveTextContent(/repository toolkit/);
    expect(await screen.findByTestId('wiki-generate-error')).toHaveTextContent(/artifact folder/);
    expect(posted).toBe(false);
  });

  it('starts a FOLDER source without a code_toolkit, and without the refusal', async () => {
    // A folder source travels in the settings themselves. The facade derives
    // the repository from `artifact_configuration` and needs no reference at
    // all, so the check that guards the repository path must not refuse it.
    const user = userEvent.setup();
    serveSlots();
    let body: Record<string, unknown> | null = null;
    server.use(
      http.post(`${BASE}/deepwiki/tools/:p/:tk/:tool/invoke`, async ({ request }) => {
        body = (await request.json()) as Record<string, unknown>;
        return HttpResponse.json({ invocation_id: 'inv-folder', status: 'Started' });
      }),
    );
    servePolls([{ status: 'InProgress' }]);
    show(
      <WikiGenerationPanel
        projectId="7"
        toolkitId="42"
        settings={{ artifact_configuration: { bucket: 'docs', prefix: 'handbook' }, llm_model: 'gpt-5' }}
        hasWiki={false}
      />,
    );
    await user.click(await screen.findByTestId('wiki-generate'));
    await waitFor(() => expect(body).not.toBeNull());
    expect(body).toMatchObject({
      configuration: { parameters: { artifact_configuration: { bucket: 'docs', prefix: 'handbook' } } },
    });
    expect(screen.queryByTestId('wiki-generate-error')).toBeNull();
  });

  it('sends ONE source: a folder drops the code_toolkit the settings still carry', async () => {
    // The facade answers 400 to a body naming both ("a wiki has one source"),
    // and a toolkit that used to name a repository still carries the
    // reference. Spreading the settings unchanged would send both.
    const user = userEvent.setup();
    serveSlots();
    let parameters: Record<string, unknown> | null = null;
    server.use(
      http.post(`${BASE}/deepwiki/tools/:p/:tk/:tool/invoke`, async ({ request }) => {
        const sent = (await request.json()) as { configuration: { parameters: Record<string, unknown> } };
        parameters = sent.configuration.parameters;
        return HttpResponse.json({ invocation_id: 'inv-folder', status: 'Started' });
      }),
    );
    servePolls([{ status: 'InProgress' }]);
    show(
      <WikiGenerationPanel
        projectId="7"
        toolkitId="42"
        settings={{ artifact_configuration: { bucket: 'docs' }, code_toolkit: 9, toolkit_configuration_code_toolkit: 9 }}
        hasWiki={false}
      />,
    );
    await user.click(await screen.findByTestId('wiki-generate'));
    await waitFor(() => expect(parameters).not.toBeNull());
    expect(parameters).toEqual({ artifact_configuration: { bucket: 'docs' } });
  });

  it('still refuses a folder this deployment will not accept', async () => {
    // A malformed block is not a source. The panel then asks for a source it
    // can use, rather than sending a body the facade refuses.
    const user = userEvent.setup();
    serveSlots();
    let posted = false;
    server.use(http.post(`${BASE}/deepwiki/tools/:p/:tk/:tool/invoke`, () => { posted = true; return HttpResponse.json({ invocation_id: 'x' }); }));
    show(
      <WikiGenerationPanel
        projectId="7"
        toolkitId="42"
        settings={{ artifact_configuration: { bucket: 'My_Docs' } }}
        hasWiki={false}
      />,
    );
    await user.click(await screen.findByTestId('wiki-generate'));
    // The message names BOTH sources: a folder is as good an answer as a
    // repository toolkit, and one that named only `code_toolkit` sent an
    // operator who had chosen a folder looking for the wrong setting.
    expect(await screen.findByTestId('wiki-generate-error')).toHaveTextContent(/repository toolkit/);
    expect(await screen.findByTestId('wiki-generate-error')).toHaveTextContent(/artifact folder/);
    expect(posted).toBe(false);
  });

  it('starts a run with the code toolkit and planner, remembers it, and follows it to completion', async () => {
    const user = userEvent.setup();
    serveSlots();
    let body: Record<string, unknown> | null = null;
    server.use(
      http.post(`${BASE}/deepwiki/tools/:p/:tk/:tool/invoke`, async ({ request }) => {
        body = (await request.json()) as Record<string, unknown>;
        return HttpResponse.json({ invocation_id: 'inv-1', status: 'Started' });
      }),
    );
    const polled = servePolls([
      { status: 'InProgress', custom_events: [{ data: { message: 'Cloning' } }] },
      { status: 'Completed', result: JSON.stringify([{ object_type: 'message', data: 'Wiki generated: 2 pages' }]) },
    ]);
    probeFetches = 0;
    show(
      <>
        <DeepWikiProbe />
        <WikiGenerationPanel projectId="7" toolkitId="42" settings={SETTINGS} hasWiki={false} />
      </>,
    );
    await user.click(await screen.findByTestId('wiki-generate'));

    await waitFor(() => expect(body).not.toBeNull());
    // Starting and polling do not refetch the wiki: nothing has landed yet.
    expect(probeFetches).toBe(1);
    expect(body).toMatchObject({
      configuration: { parameters: { code_toolkit: 9, repository: 'acme/svc' } },
      parameters: { query: 'GO', planner_type: 'cluster', exclude_tests: true },
    });
    // Remembered while running...
    expect(createGenerationStorage('7', '42').load()?.invocationId).toBe('inv-1');
    await waitFor(() => expect(screen.getByTestId('wiki-generation-status')).toHaveAttribute('data-status', 'completed'), { timeout: 10_000 });
    expect(polled.length).toBeGreaterThanOrEqual(1);
    // Completion invalidates the `deepwiki` root, so the pages that just landed are read again.
    await waitFor(() => expect(probeFetches).toBe(2));
    // ...and forgotten once settled, so a reload does not poll a finished run.
    expect(createGenerationStorage('7', '42').load()).toBeNull();
  });

  it('RESUMES a remembered run on mount instead of showing idle', async () => {
    // DWIKI-006: the page was reloaded while a generation ran.
    serveSlots();
    createGenerationStorage('7', '42').save({ invocationId: 'inv-stored', startedAt: Date.now() });
    const polled = servePolls([{ status: 'InProgress', custom_events: [{ data: { message: 'still going' } }] }]);
    show(<WikiGenerationPanel projectId="7" toolkitId="42" settings={SETTINGS} hasWiki />);
    await waitFor(() => expect(polled).toContain('inv-stored'));
    expect(screen.getByTestId('wiki-generate-stop')).toBeVisible();
  });

  it('stops a run with the facade DELETE', async () => {
    const user = userEvent.setup();
    serveSlots();
    createGenerationStorage('7', '42').save({ invocationId: 'inv-stored', startedAt: Date.now() });
    servePolls([{ status: 'InProgress' }]);
    let cancelled: string | null = null;
    server.use(
      http.delete(`${BASE}/deepwiki/invocations/:projectId/:toolkit/:tool/:invocation`, ({ params }) => {
        cancelled = String(params['invocation']);
        return new HttpResponse(null, { status: 204 });
      }),
    );
    show(<WikiGenerationPanel projectId="7" toolkitId="42" settings={SETTINGS} hasWiki />);
    await user.click(await screen.findByTestId('wiki-generate-stop'));
    await waitFor(() => expect(cancelled).toBe('inv-stored'));
  });

  it('disables Generate when no slot is free', async () => {
    serveSlots(false);
    show(<WikiGenerationPanel projectId="7" toolkitId="42" settings={SETTINGS} hasWiki={false} />);
    await waitFor(() => expect(screen.getByTestId('wiki-generate')).toBeDisabled());
    expect(screen.getByTestId('wiki-generate')).toHaveTextContent(/slots busy/i);
  });

  it('asks before regenerating an existing wiki', async () => {
    const user = userEvent.setup();
    serveSlots();
    let posted = false;
    server.use(http.post(`${BASE}/deepwiki/tools/:p/:tk/:tool/invoke`, () => { posted = true; return HttpResponse.json({ invocation_id: 'inv-2' }); }));
    servePolls([{ status: 'InProgress' }]);
    show(<WikiGenerationPanel projectId="7" toolkitId="42" settings={SETTINGS} hasWiki />);
    await user.click(await screen.findByTestId('wiki-generate'));
    expect(posted).toBe(false);
    await user.click(screen.getByTestId('wiki-generate-confirm'));
    await waitFor(() => expect(posted).toBe(true));
  });
});
