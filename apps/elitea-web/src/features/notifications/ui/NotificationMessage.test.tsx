import { describe, expect, it, vi } from 'vitest';

import { screen } from '@testing-library/react';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import type { NormalizedNotification } from '../api/normalize';
import { NotificationMessage } from './NotificationMessage';

function notification(overrides: Partial<NormalizedNotification> = {}): NormalizedNotification {
  return {
    id: '1',
    eventType: 'chat_user_added',
    createdAt: '2026-01-01T00:00:00Z',
    isSeen: false,
    ...overrides,
  };
}

describe('NotificationMessage', () => {
  it('renders plain text when meta.message has no [text]() link segments', () => {
    renderWithTheme(<NotificationMessage notification={notification({ meta: { message: 'plain text here' } })} />);
    expect(screen.getByText('plain text here')).toBeInTheDocument();
  });

  it('renders a link for a [text]() segment when the href resolves', () => {
    renderWithTheme(
      <NotificationMessage
        notification={notification({
          meta: { message: '[Chat]() was updated', conversationId: 'c1' },
          projectId: '7',
        })}
      />,
    );
    const link = screen.getByRole('link', { name: 'Chat' });
    expect(link).toHaveAttribute('target', '_blank');
    expect(link.getAttribute('href')).toContain('/7/chat?conversation=c1');
  });

  it('renders link text as plain text (no anchor) when the href does not resolve', () => {
    renderWithTheme(
      <NotificationMessage notification={notification({ eventType: 'rates', meta: { message: '[link text]()' } })} />,
    );
    expect(screen.queryByRole('link')).toBeNull();
    expect(screen.getByText('link text')).toBeInTheDocument();
  });

  it('falls back to LegacyNotificationMessage when meta.message is absent', () => {
    renderWithTheme(
      <NotificationMessage
        notification={notification({
          eventType: 'private_project_created',
          meta: {},
        })}
      />,
    );
    expect(screen.getByText('Project was successfully created.')).toBeInTheDocument();
  });

  it('renders nothing (LegacyNotificationMessage returns null) when meta itself is absent, per that component\'s deliberate safety guard', () => {
    const { container } = renderWithTheme(
      <NotificationMessage notification={notification({ eventType: 'private_project_created' })} />,
    );
    expect(container).toBeEmptyDOMElement();
  });

  it('forwards onCloseNotificationList to the legacy fallback, not to its own [text]() links (NotificationListItemMessage.jsx:17-40 has no onClick on that path — new-tab links leave the popover open)', () => {
    const onClose = vi.fn();
    renderWithTheme(
      <NotificationMessage
        notification={notification({
          meta: { message: '[Chat]() updated', conversationId: 'c1' },
          projectId: '7',
        })}
        onCloseNotificationList={onClose}
      />,
    );
    const link = screen.getByRole('link', { name: 'Chat' });
    expect(link).not.toHaveAttribute('onclick');
    expect(onClose).not.toHaveBeenCalled();
  });

  it('names an event type the legacy renderer has no text for instead of an empty cell (#1992)', () => {
    renderWithTheme(
      <NotificationMessage
        notification={notification({ eventType: 'agent_approval_required' as NormalizedNotification['eventType'], meta: {} })}
      />,
    );
    expect(screen.getByText('Agent approval required')).toBeInTheDocument();
  });
});
