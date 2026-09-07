import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { renderPipelinesRoute } from '../__tests__/testRouter';
import { EditPipelineTriggersPanel } from './EditPipelineTriggersPanel';

/**
 * The pipeline's "Triggers & schedules" section — issues 192 and 193.
 *
 * Every assertion here is about what a person can SEE and DO, driven through
 * msw against the real generated client, because the two defects this section
 * is most likely to ship are both invisible to a shallow render:
 *
 *   - the credential leaking into the read that runs on every open, which is
 *     a rendering fact and not a network one;
 *   - the response envelope read one level too shallow, which compiles, gives
 *     `undefined` on a 200, and paints the "no trigger" empty state over a
 *     configured pipeline. That exact shape has shipped twice here (issue
 *     132), so the fixtures below are RAW bodies — what the server sends —
 *     and never a hand-built `{data: …}`.
 */

const CONFIGURED_TRIGGER = {
  configured: true,
  token_id: 'ab12cd34',
  url: '/api/v2/pipeline_trigger/9/ab12cd34',
  created_by: 6,
  created_at: '2026-02-01T10:00:00Z',
  last_used_at: '2026-02-02T11:30:00Z',
};

const NO_TRIGGER = { configured: false };
const NO_SCHEDULE = { configured: false, active: false };

const CONFIGURED_SCHEDULE = {
  configured: true,
  cron: '0 3 * * *',
  active: true,
  author_id: 6,
  next_run: '2026-02-03T03:00:00Z',
  last_result: 'skipped_overlap',
  last_result_detail: 'the previous run has not finished',
};

interface Fixtures {
  readonly trigger?: unknown;
  readonly schedule?: unknown;
}

/** Records what the section asked for, so a test can assert the write actually left. */
interface Recorded {
  rotated: number;
  revoked: number;
  revealed: number;
  saved: unknown[];
  deleted: number;
}

function serve(fixtures: Fixtures = {}): Recorded {
  const recorded: Recorded = { rotated: 0, revoked: 0, revealed: 0, saved: [], deleted: 0 };
  server.use(
    http.get('*/pipeline_triggers/secret/prompt_lib/:projectId/:versionId', () => {
      recorded.revealed += 1;
      return HttpResponse.json({
        ...CONFIGURED_TRIGGER,
        secret: 'the-secret',
        secret_url: `${CONFIGURED_TRIGGER.url}?token=the-secret`,
      });
    }),
    http.get('*/pipeline_triggers/prompt_lib/:projectId/:versionId', () =>
      HttpResponse.json(fixtures.trigger ?? NO_TRIGGER),
    ),
    http.post('*/pipeline_triggers/prompt_lib/:projectId/:versionId', () => {
      recorded.rotated += 1;
      return HttpResponse.json({
        ...CONFIGURED_TRIGGER,
        secret: 'fresh-secret',
        secret_url: `${CONFIGURED_TRIGGER.url}?token=fresh-secret`,
      });
    }),
    http.delete('*/pipeline_triggers/prompt_lib/:projectId/:versionId', () => {
      recorded.revoked += 1;
      return HttpResponse.json({ ...CONFIGURED_TRIGGER, revoked_at: '2026-02-03T09:00:00Z' });
    }),
    http.get('*/pipeline_schedules/prompt_lib/:projectId/:versionId', () =>
      HttpResponse.json(fixtures.schedule ?? NO_SCHEDULE),
    ),
    http.put('*/pipeline_schedules/prompt_lib/:projectId/:versionId', async ({ request }) => {
      recorded.saved.push(await request.json());
      return HttpResponse.json(CONFIGURED_SCHEDULE);
    }),
    http.delete('*/pipeline_schedules/prompt_lib/:projectId/:versionId', () => {
      recorded.deleted += 1;
      return HttpResponse.json(NO_SCHEDULE);
    }),
  );
  return recorded;
}

function renderPanel(isReadOnly = false): void {
  renderPipelinesRoute(
    <EditPipelineTriggersPanel projectId="9" versionId={1} isReadOnly={isReadOnly} />,
    '/pipelines/all/42',
    { projectId: '9' },
  );
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
});

afterEach(() => {
  resetGeneratedClient();
});

describe('EditPipelineTriggersPanel', () => {
  it('renders nothing until the pipeline version is resolved', () => {
    serve();
    renderPipelinesRoute(
      <EditPipelineTriggersPanel projectId="9" versionId={undefined} isReadOnly={false} />,
      '/pipelines/all/42',
      { projectId: '9' },
    );
    // Offering "create trigger URL" for a version the page does not have yet
    // would be an action whose first click cannot work.
    expect(screen.queryByTestId('edit-pipeline-triggers-panel')).not.toBeInTheDocument();
  });

  it('shows a configured trigger, and its URL carries NO credential', async () => {
    serve({ trigger: CONFIGURED_TRIGGER });
    renderPanel();

    const url = await screen.findByTestId('pipeline-trigger-url');
    expect(url).toHaveTextContent(CONFIGURED_TRIGGER.url);
    // The read that runs on every open must not carry the secret: it would sit
    // in the query cache and in any exported HAR.
    expect(url.textContent).not.toContain('token=');
    expect(screen.queryByTestId('pipeline-trigger-absent')).not.toBeInTheDocument();
    expect(await screen.findByTestId('pipeline-trigger-last-used')).toBeInTheDocument();
  });

  it('says so when the pipeline has no trigger', async () => {
    serve();
    renderPanel();

    expect(await screen.findByTestId('pipeline-trigger-absent')).toBeInTheDocument();
    expect(screen.getByTestId('pipeline-trigger-rotate')).toHaveTextContent('Create trigger URL');
  });

  it('reveals the credential only when asked, and hides it again', async () => {
    const user = userEvent.setup();
    const recorded = serve({ trigger: CONFIGURED_TRIGGER });
    renderPanel();

    await user.click(await screen.findByTestId('pipeline-trigger-reveal'));
    await waitFor(() => expect(screen.getByTestId('pipeline-trigger-url')).toHaveTextContent('token=the-secret'));
    expect(recorded.revealed).toBe(1);

    await user.click(screen.getByTestId('pipeline-trigger-hide'));
    await waitFor(() =>
      expect(screen.getByTestId('pipeline-trigger-url').textContent).not.toContain('token='),
    );
  });

  it('rotating asks the server and shows the new credential once', async () => {
    const user = userEvent.setup();
    const recorded = serve({ trigger: CONFIGURED_TRIGGER });
    renderPanel();

    await user.click(await screen.findByTestId('pipeline-trigger-rotate'));
    await waitFor(() => expect(recorded.rotated).toBe(1));
    await waitFor(() => expect(screen.getByTestId('pipeline-trigger-url')).toHaveTextContent('token=fresh-secret'));
  });

  it('revoking asks the server and drops the shown credential', async () => {
    const user = userEvent.setup();
    const recorded = serve({ trigger: CONFIGURED_TRIGGER });
    renderPanel();

    await user.click(await screen.findByTestId('pipeline-trigger-reveal'));
    await waitFor(() => expect(screen.getByTestId('pipeline-trigger-url')).toHaveTextContent('token=the-secret'));

    await user.click(screen.getByTestId('pipeline-trigger-revoke'));
    await waitFor(() => expect(recorded.revoked).toBe(1));
    await waitFor(() =>
      expect(screen.getByTestId('pipeline-trigger-url').textContent).not.toContain('token='),
    );
  });

  it('shows the stored schedule, its next run and why the last fire was skipped', async () => {
    serve({ schedule: CONFIGURED_SCHEDULE });
    renderPanel();

    expect(await screen.findByTestId('pipeline-schedule-next-run')).toBeInTheDocument();
    // The outcome vocabulary is closed on both sides. An unmapped value would
    // render as nothing, which is the empty state this column exists to remove.
    expect(await screen.findByTestId('pipeline-schedule-last-result')).toHaveTextContent(
      'Skipped: the previous run had not finished.',
    );
  });

  // Every value the backend's CHECK constraint admits, plus one it does not.
  // The set is closed on both sides, and an unmapped outcome rendering as
  // nothing is the empty state this column exists to remove — so the fallback
  // has to show the server's own sentence rather than disappear.
  it.each([
    ['dispatched', 'The last run started.'],
    ['skipped_overlap', 'Skipped: the previous run had not finished.'],
    ['skipped_unauthorized', 'Skipped: the schedule author can no longer run agents in this project.'],
    ['skipped_missing_version', 'Skipped: this pipeline version is gone.'],
    ['failed', 'The last run could not start.'],
    ['a_result_this_build_does_not_know', 'the server said why'],
  ])('renders the %s outcome', async (lastResult, expected) => {
    serve({
      schedule: { ...CONFIGURED_SCHEDULE, last_result: lastResult, last_result_detail: 'the server said why' },
    });
    renderPanel();

    expect(await screen.findByTestId('pipeline-schedule-last-result')).toHaveTextContent(expected);
  });

  it('shows no outcome line for a schedule that has never fired', async () => {
    serve({ schedule: { configured: true, cron: '0 3 * * *', active: false, author_id: 6 } });
    renderPanel();

    await screen.findByTestId('pipeline-schedule-card');
    expect(screen.queryByTestId('pipeline-schedule-last-result')).not.toBeInTheDocument();
    // No `next_run` in the fixture either — an absent preview must not render
    // as an invented one.
    expect(screen.queryByTestId('pipeline-schedule-next-run')).not.toBeInTheDocument();
  });

  it('copies the revealed URL to the clipboard', async () => {
    const user = userEvent.setup();
    const written: string[] = [];
    Object.defineProperty(navigator, 'clipboard', {
      configurable: true,
      value: {
        writeText: (text: string) => {
          written.push(text);
          return Promise.resolve();
        },
      },
    });
    serve({ trigger: CONFIGURED_TRIGGER });
    renderPanel();

    await user.click(await screen.findByTestId('pipeline-trigger-reveal'));
    await waitFor(() => expect(screen.getByTestId('pipeline-trigger-copy')).toBeInTheDocument());
    await user.click(screen.getByTestId('pipeline-trigger-copy'));

    await waitFor(() => expect(written).toEqual([`${CONFIGURED_TRIGGER.url}?token=the-secret`]));
  });

  it('saves the cron the person edited, with the enabled flag', async () => {
    const user = userEvent.setup();
    const recorded = serve({ schedule: CONFIGURED_SCHEDULE });
    renderPanel();

    await user.click(await screen.findByTestId('pipeline-schedule-save'));
    await waitFor(() => expect(recorded.saved).toHaveLength(1));
    expect(recorded.saved[0]).toEqual({ cron: '0 3 * * *', active: true });
  });

  it('removes the schedule', async () => {
    const user = userEvent.setup();
    const recorded = serve({ schedule: CONFIGURED_SCHEDULE });
    renderPanel();

    await user.click(await screen.findByTestId('pipeline-schedule-remove'));
    await waitFor(() => expect(recorded.deleted).toBe(1));
  });

  it('offers no write to a read-only viewer', async () => {
    serve({ trigger: CONFIGURED_TRIGGER, schedule: CONFIGURED_SCHEDULE });
    renderPanel(true);

    // Wait for the LOADED state first. `rotate` renders before the query
    // settles, so asserting on it alone would pass against an empty section
    // and say nothing about the buttons a configured trigger offers.
    await screen.findByTestId('pipeline-trigger-url');
    expect(screen.getByTestId('pipeline-trigger-rotate')).toBeDisabled();
    expect(screen.getByTestId('pipeline-trigger-reveal')).toBeDisabled();
    expect(screen.getByTestId('pipeline-trigger-revoke')).toBeDisabled();
    expect(screen.getByTestId('pipeline-schedule-save')).toBeDisabled();
    expect(screen.getByTestId('pipeline-schedule-remove')).toBeDisabled();
  });

  it('reports a failed write instead of failing silently', async () => {
    const user = userEvent.setup();
    serve({ trigger: CONFIGURED_TRIGGER });
    server.use(
      http.delete('*/pipeline_triggers/prompt_lib/:projectId/:versionId', () =>
        HttpResponse.json({ error: 'nope' }, { status: 500 }),
      ),
    );
    renderPanel();

    await user.click(await screen.findByTestId('pipeline-trigger-revoke'));
    expect(await screen.findByTestId('pipeline-triggers-error')).toBeInTheDocument();
  });
});
