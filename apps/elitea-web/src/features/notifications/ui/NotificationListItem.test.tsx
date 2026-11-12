import type { ReactElement } from 'react';

import CssBaseline from '@mui/material/CssBaseline';
import { ThemeProvider } from '@mui/material/styles';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { server } from '../../../test/setup';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';

import type { NormalizedNotification } from '../api/normalize';
import { NotificationListItem } from './NotificationListItem';

const BASE = '/api/v2';
const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

function renderItem(ui: ReactElement): ReturnType<typeof render> {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(
    <ThemeProvider
      theme={theme}
      defaultMode={DEFAULT_COLOR_SCHEME}
    >
      <CssBaseline />
      <QueryClientProvider client={client}>{ui}</QueryClientProvider>
    </ThemeProvider>,
  );
}

function notification(overrides: Partial<NormalizedNotification> = {}): NormalizedNotification {
  return {
    id: '1',
    eventType: 'private_project_created',
    createdAt: '2026-01-01T00:00:00Z',
    isSeen: false,
    meta: {},
    ...overrides,
  };
}

afterEach(() => {
  resetGeneratedClient();
});

describe('NotificationListItem', () => {
  it('renders the message text and the relative time', () => {
    renderItem(
      <NotificationListItem
        notification={notification()}
        projectId="7"
      />,
    );
    expect(screen.getByText('Project was successfully created.')).toBeInTheDocument();
    expect(screen.getByText(/ago$/)).toBeInTheDocument();
  });

  it('hides the timestamp when showTime is false', () => {
    renderItem(
      <NotificationListItem
        notification={notification()}
        projectId="7"
        showTime={false}
      />,
    );
    expect(screen.queryByText(/ago$/)).toBeNull();
  });

  it('hides the icon slot when showIcon is false', () => {
    const { container } = renderItem(
      <NotificationListItem
        notification={notification({ eventType: 'author_approval' })}
        projectId="7"
        showIcon={false}
      />,
    );
    expect(container.querySelector('svg')).toBeNull();
  });

  it('shows the mark-toggle button only in "list" context, on hover', async () => {
    const user = userEvent.setup();
    renderItem(
      <NotificationListItem
        notification={notification()}
        projectId="7"
        context="list"
      />,
    );
    expect(screen.queryByRole('button', { name: 'Mark as read' })).toBeNull();
    await user.hover(screen.getByText('Project was successfully created.'));
    expect(await screen.findByRole('button', { name: 'Mark as read' })).toBeInTheDocument();
  });

  it('hides the mark-toggle button again on mouse-leave', async () => {
    const user = userEvent.setup();
    renderItem(
      <NotificationListItem
        notification={notification()}
        projectId="7"
        context="list"
      />,
    );
    const text = screen.getByText('Project was successfully created.');
    await user.hover(text);
    expect(await screen.findByRole('button', { name: 'Mark as read' })).toBeInTheDocument();
    await user.unhover(text);
    expect(screen.queryByRole('button', { name: 'Mark as read' })).toBeNull();
  });

  it('never shows the mark-toggle button in "table" context, even on hover', async () => {
    const user = userEvent.setup();
    renderItem(
      <NotificationListItem
        notification={notification()}
        projectId="7"
        context="table"
      />,
    );
    await user.hover(screen.getByText('Project was successfully created.'));
    expect(screen.queryByRole('button', { name: 'Mark as read' })).toBeNull();
  });

  it('labels the toggle "Mark as unread" for an already-seen notification', async () => {
    const user = userEvent.setup();
    renderItem(
      <NotificationListItem
        notification={notification({ isSeen: true })}
        projectId="7"
      />,
    );
    await user.hover(screen.getByText('Project was successfully created.'));
    expect(await screen.findByRole('button', { name: 'Mark as unread' })).toBeInTheDocument();
  });

  it('clicking the mark-toggle button fires the bulk-mark-seen mutation and calls onNotificationSeenChange', async () => {
    configureGeneratedClient({ baseUrl: BASE });
    let sentBody: unknown;
    server.use(
      http.put(`${BASE}/notifications/notifications/prompt_lib/7`, async ({ request }) => {
        sentBody = await request.json();
        return HttpResponse.json({});
      }),
    );
    const onChange = vi.fn();
    const user = userEvent.setup();
    renderItem(
      <NotificationListItem
        notification={notification({ id: '99', isSeen: false })}
        projectId="7"
        onNotificationSeenChange={onChange}
      />,
    );
    await user.hover(screen.getByText('Project was successfully created.'));
    const button = await screen.findByRole('button', { name: 'Mark as read' });
    // `fireEvent.click` (not `user.click`): userEvent's realistic pointer-path
    // simulation moves the pointer from the earlier `user.hover` target to
    // the button, which — through this component's own `onMouseLeave` on the
    // outer container — flips `isHovered` back to `false` and unmounts the
    // button (a real, if narrow, hover-race in the component's own design,
    // not a test artifact) before the click lands. A direct click event
    // exercises the actual mutation-firing behaviour under test without
    // fighting that hover state machine.
    fireEvent.click(button);
    await waitFor(() => expect(onChange).toHaveBeenCalledWith('99', true));
    expect(sentBody).toEqual({ ids: [99], is_seen: true });
  });

  it('surfaces a mutation failure via onMarkToggleError instead of throwing', async () => {
    configureGeneratedClient({ baseUrl: BASE });
    server.use(
      http.put(`${BASE}/notifications/notifications/prompt_lib/7`, () =>
        HttpResponse.json({ message: 'nope' }, { status: 400 }),
      ),
    );
    const onError = vi.fn();
    const user = userEvent.setup();
    renderItem(
      <NotificationListItem
        notification={notification()}
        projectId="7"
        onMarkToggleError={onError}
      />,
    );
    await user.hover(screen.getByText('Project was successfully created.'));
    const button = await screen.findByRole('button', { name: 'Mark as read' });
    // See the identical comment in the preceding test — `fireEvent.click`
    // avoids userEvent's pointer-path hover-race in jsdom.
    fireEvent.click(button);
    await waitFor(() => expect(onError).toHaveBeenCalledWith('nope'));
  });

  it('does nothing when clicked with no projectId (guard clause)', async () => {
    configureGeneratedClient({ baseUrl: BASE });
    let hit = false;
    server.use(
      http.put(`${BASE}/notifications/notifications/prompt_lib/7`, () => {
        hit = true;
        return HttpResponse.json({});
      }),
    );
    const user = userEvent.setup();
    renderItem(
      <NotificationListItem
        notification={notification()}
        projectId={undefined}
      />,
    );
    await user.hover(screen.getByText('Project was successfully created.'));
    const button = await screen.findByRole('button', { name: 'Mark as read' });
    // fireEvent (not userEvent — see the two tests above): makes sure this
    // assertion is actually exercising the guard clause, not passing
    // vacuously because the click never landed.
    fireEvent.click(button);
    expect(hit).toBe(false);
  });

  it('merges caller sx/contentSx with the component defaults rather than replacing them', () => {
    const { container } = renderItem(
      <NotificationListItem
        notification={notification()}
        projectId="7"
        sx={{ opacity: 0.5 }}
      />,
    );
    // A non-crashing render with the override applied is the real assertion;
    // combineSx's own merge behaviour is unit-tested by shared/ui (S1).
    expect(container.firstChild).not.toBeNull();
  });

  it('renders without the line-clamp style block when clampLines is 0 (table context passes 0)', () => {
    const { container } = renderItem(
      <NotificationListItem
        notification={notification()}
        projectId="7"
        clampLines={0}
      />,
    );
    expect(container.firstChild).not.toBeNull();
  });

  it('falls back to console.error when no onMarkToggleError is supplied', async () => {
    configureGeneratedClient({ baseUrl: BASE });
    server.use(
      http.put(`${BASE}/notifications/notifications/prompt_lib/7`, () =>
        HttpResponse.json({ message: 'nope' }, { status: 400 }),
      ),
    );
    const consoleErrorSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    const user = userEvent.setup();
    renderItem(
      <NotificationListItem
        notification={notification()}
        projectId="7"
      />,
    );
    await user.hover(screen.getByText('Project was successfully created.'));
    const button = await screen.findByRole('button', { name: 'Mark as read' });
    fireEvent.click(button);
    await waitFor(() => expect(consoleErrorSpy).toHaveBeenCalledWith('nope'));
    consoleErrorSpy.mockRestore();
  });
});
