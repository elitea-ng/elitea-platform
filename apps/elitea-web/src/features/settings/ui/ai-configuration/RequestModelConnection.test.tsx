/**
 * RequestModelConnection.test.tsx
 *
 * What this file pins is the WIRE, because that is the whole feature: there is
 * no new route and no new column, so "it works" means one POST to the App
 * Catalogue's own create call carrying the two values that make it a model
 * connection instead of an app-access request.
 *
 * Two of the assertions below exist because the failures they catch are
 * silent:
 *
 *   - a request that carried a different `issue_type` would still be created,
 *     answered 201, and land in the operator's queue — in the WRONG queue, and
 *     nothing on either screen would say so;
 *   - a model name containing a slash (`meta-llama/Llama-3.1-70B`, and every
 *     other vendor-prefixed id) would split the path segment `entity_id`
 *     travels in, so the POST would address a route that does not exist. The
 *     dialog would report a failure with no hint of the cause.
 */
import CssBaseline from '@mui/material/CssBaseline';
import { ThemeProvider } from '@mui/material/styles';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';

import { server } from '../../../../test/setup';

import {
  MODEL_CONNECTION_ISSUE_TYPE,
  RequestModelConnection,
  buildModelConnectionEntityId,
} from './RequestModelConnection';
import { testTimeout } from '@/test/timeouts';

const BASE = '/api/v2';
const PROJECT_ID = '1';

interface CapturedRequest {
  url: string;
  body: unknown;
}

/** Every POST the component made, in order — `length` is as load-bearing as the contents. */
let captured: CapturedRequest[] = [];

function mockCreate(status = 201): void {
  server.use(
    http.post('*/admin/moderation_status/default/:projectId/:entityId', async ({ request }) => {
      captured.push({ url: request.url, body: await request.json() });
      if (status !== 201) {
        return HttpResponse.json({ error: 'forbidden' }, { status });
      }
      return HttpResponse.json({ id: 1, status: 'pending' }, { status: 201 });
    }),
  );
}

function renderRequestButton(): void {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);
  render(
    <QueryClientProvider client={queryClient}>
      <ThemeProvider theme={theme} defaultMode={DEFAULT_COLOR_SCHEME}>
        <CssBaseline />
        <RequestModelConnection projectId={PROJECT_ID} />
      </ThemeProvider>
    </QueryClientProvider>,
  );
}

/**
 * The dialog is addressed by its `dialog` role, not by its heading: BaseModal
 * puts the close button inside `DialogTitle`, so the heading's accessible name
 * is the title plus "Close".
 */
async function openDialog(): Promise<void> {
  await userEvent.click(screen.getByRole('button', { name: 'Request a model connection' }));
  await screen.findByRole('dialog');
}

/** The path segment `entity_id` travelled in, decoded from the captured URL. */
function entitySegmentOf(request: CapturedRequest): string {
  const segments = new URL(request.url, 'http://localhost').pathname.split('/');
  return segments[segments.length - 1] ?? '';
}

beforeEach(() => {
  captured = [];
  configureGeneratedClient({ baseUrl: BASE });
});

afterEach(() => {
  resetGeneratedClient();
});

describe('buildModelConnectionEntityId', () => {
  it('addresses a provider and a model under distinct, readable prefixes', () => {
    expect(buildModelConnectionEntityId('provider', 'anthropic')).toBe('provider:anthropic');
    expect(buildModelConnectionEntityId('model', 'claude-opus-4')).toBe('model:claude-opus-4');
  });

  /* The colon stays literal on purpose: it is legal in a path segment, and the
     admin queue renders this value to a human. Only the name is encoded. */
  it('encodes the name but not the prefix', () => {
    expect(buildModelConnectionEntityId('model', 'meta-llama/Llama-3.1-70B')).toBe(
      'model:meta-llama%2FLlama-3.1-70B',
    );
    expect(buildModelConnectionEntityId('provider', '  azure openai  ')).toBe('provider:azure%20openai');
  });
});

/* This file drives a MUI dialog through ~50 keystrokes and three async
   settles per test, and vitest's default 5000ms is a generic value rather than
   a budget anybody measured against that work. On CI the siblings run at
   3.7-3.8s and "files a provider request..." tipped over 5005ms — it has now
   failed on three different branches while passing on the same code elsewhere,
   which is a budget set too close to the work, not a defect in the component.

   Both halves of the fix are here: the `delay: null` on each userEvent.type
   removes the per-keystroke await (measured: tests 2.26s -> 1.97s locally),
   and this raises the ceiling to something the work fits under on a loaded
   runner. Neither weakens an assertion. */
vi.setConfig({ testTimeout: testTimeout(20_000) });

describe('RequestModelConnection', () => {
  it('files a provider request on the catalogue create route, with only the two fields a requester owns', async () => {
    mockCreate();
    renderRequestButton();
    await openDialog();

    await userEvent.type(screen.getByLabelText('Provider type *'), 'anthropic', { delay: null });
    await userEvent.type(screen.getByLabelText('Description *'), 'We need Claude for the review pipeline.', { delay: null });
    await userEvent.click(screen.getByRole('button', { name: 'Send request' }));

    await waitFor(() => expect(captured).toHaveLength(1));
    const request = captured[0];
    expect(request).toBeDefined();
    expect(entitySegmentOf(request as CapturedRequest)).toBe('provider:anthropic');
    expect(new URL((request as CapturedRequest).url, 'http://localhost').pathname).toContain(
      `/admin/moderation_status/default/${PROJECT_ID}/`,
    );

    /* `issue_type` is the ONLY thing separating this from the app-access
       queue, and `status`/`user_id`/`meta` are refused by the server — sending
       them would be a 400, not a stricter request. */
    expect((request as CapturedRequest).body).toEqual({
      issue_type: MODEL_CONNECTION_ISSUE_TYPE,
      description: 'We need Claude for the review pipeline.',
    });

    /* Filed, said so, and got out of the way. */
    expect(await screen.findByText('Your model connection request has been sent')).toBeInTheDocument();
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
  });

  it('addresses a model request under the model: prefix, and relabels its field', async () => {
    mockCreate();
    renderRequestButton();
    await openDialog();

    await userEvent.click(screen.getByRole('radio', { name: 'A model' }));
    /* The field is about a different thing now, and says so — asking for a
       "provider type" and getting a model name back is how the two end up in
       one column. */
    expect(screen.queryByLabelText('Provider type *')).not.toBeInTheDocument();

    await userEvent.type(screen.getByLabelText('Model name *'), 'claude-opus-4', { delay: null });
    await userEvent.type(screen.getByLabelText('Description *'), 'Needed for long-context review.', { delay: null });
    await userEvent.click(screen.getByRole('button', { name: 'Send request' }));

    await waitFor(() => expect(captured).toHaveLength(1));
    expect(entitySegmentOf(captured[0] as CapturedRequest)).toBe('model:claude-opus-4');
  });

  it('keeps a slashed model name inside one path segment', async () => {
    mockCreate();
    renderRequestButton();
    await openDialog();

    await userEvent.click(screen.getByRole('radio', { name: 'A model' }));
    await userEvent.type(screen.getByLabelText('Model name *'), 'meta-llama/Llama-3.1-70B', { delay: null });
    await userEvent.type(screen.getByLabelText('Description *'), 'Self-hosted inference.', { delay: null });
    await userEvent.click(screen.getByRole('button', { name: 'Send request' }));

    await waitFor(() => expect(captured).toHaveLength(1));
    /* Raw, not decoded: the point is that the slash never reached the router
       as a separator. A decoded read here would pass against the bug. */
    expect((captured[0] as CapturedRequest).url).toContain('model:meta-llama%2FLlama-3.1-70B');
  });

  it('refuses an empty form with a reason per field, and posts nothing', async () => {
    mockCreate();
    renderRequestButton();
    await openDialog();

    await userEvent.click(screen.getByRole('button', { name: 'Send request' }));

    /* Both at once. Revealing the second problem only after the first is fixed
       costs a round trip per field. */
    expect(await screen.findByText('Name what should be connected')).toBeInTheDocument();
    expect(screen.getByText('Describe why this project needs it')).toBeInTheDocument();
    expect(captured).toHaveLength(0);

    /* Whitespace is not an answer either — the server trims and would refuse
       it with a 400 the user cannot act on. */
    await userEvent.type(screen.getByLabelText('Provider type *'), '   ', { delay: null });
    await userEvent.click(screen.getByRole('button', { name: 'Send request' }));
    expect(captured).toHaveLength(0);
  });

  it('keeps the dialog and what was typed when the send fails', async () => {
    mockCreate(403);
    renderRequestButton();
    await openDialog();

    await userEvent.type(screen.getByLabelText('Provider type *'), 'anthropic', { delay: null });
    await userEvent.type(screen.getByLabelText('Description *'), 'We need Claude.', { delay: null });
    await userEvent.click(screen.getByRole('button', { name: 'Send request' }));

    await waitFor(() => expect(captured).toHaveLength(1));
    expect(await screen.findByText('The request could not be sent')).toBeInTheDocument();

    /* A 403 here means the caller lacks `admin.moderation.create`, which the
       UI cannot know in advance. Closing the dialog would throw away the text
       AND leave the failure unexplained. */
    expect(screen.getByRole('dialog')).toBeInTheDocument();
    expect(screen.getByLabelText('Provider type *')).toHaveValue('anthropic');
  });
});
