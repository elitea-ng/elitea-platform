import { render, screen } from '@testing-library/react-native';
import { StyleSheet } from 'react-native';

import { DEFAULT_A11Y, resolveDesign } from '@/design/resolve';
import { NotificationRow, type InboxRow } from '@/features/notifications/NotificationsView';

// The RN jest window reports font scale 2; this file pins the default text size instead.
const mockDesign = resolveDesign({ scheme: 'light', platform: 'ios', a11y: { ...DEFAULT_A11Y, fontScale: 1 } });
jest.mock('@/design/DesignProvider', () => ({
  ...jest.requireActual('@/design/DesignProvider'),
  useDesign: () => mockDesign,
}));

const row: InboxRow = {
  id: 7,
  event_type: 'agent_finished',
  meta: {},
  is_seen: false,
  created_at: '2026-10-04T11:55:00Z',
  age: '5m',
};

it('at the default text size a row stays one line: symbol, title column, age (S2)', async () => {
  await render(<NotificationRow row={row} onOpen={jest.fn()} />);
  expect(mockDesign.accessibilityLayout).toBe(false);
  expect(StyleSheet.flatten(screen.getByTestId('notification-7-body').props.style)).toMatchObject({ flexDirection: 'row' });
});
