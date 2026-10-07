import { fireEvent, render, screen } from '@testing-library/react-native';
import { StyleSheet } from 'react-native';

import { DEFAULT_A11Y, resolveDesign, type Design } from '@/design/resolve';
import { sampleDevices } from '@/features/designGallery/secondaryFixtures';
import { DevicesView, type DevicesViewProps } from '@/features/settings/DevicesView';
import { formatAge } from '@/lib/time';

// The RN jest window reports font scale 2 (an accessibility size); `mockDesign` pins another.
let mockDesign: Design | null = null;
jest.mock('@/design/DesignProvider', () => {
  const actual = jest.requireActual('@/design/DesignProvider');
  return { ...actual, useDesign: () => mockDesign ?? actual.useDesign() };
});

const NOW = Date.UTC(2026, 9, 7, 12);
const devices = sampleDevices(NOW);

function props(over: Partial<DevicesViewProps> = {}): DevicesViewProps {
  return {
    status: { kind: 'ready' },
    devices,
    host: 'ai.example.com',
    age: (iso) => formatAge(iso, NOW),
    banner: null,
    onRevoke: jest.fn(async () => true),
    onRetry: jest.fn(),
    ...over,
  };
}

const layoutOf = (id: string) => StyleSheet.flatten(screen.getByTestId(`device-${id}-body`).props.style);

beforeEach(() => {
  mockDesign = null;
});

describe('device rows (S3)', () => {
  it('at accessibility sizes every row stacks the same way, whatever the length of its detail', async () => {
    await render(<DevicesView {...props()} />);
    for (const device of devices) {
      expect(layoutOf(device.id).flexDirection).not.toBe('row');
      expect(layoutOf(device.id).flexWrap).toBeUndefined();
    }
  });

  it('at the default size every row is one line: symbol, name and platform, last activity', async () => {
    mockDesign = resolveDesign({ scheme: 'light', platform: 'ios', a11y: { ...DEFAULT_A11Y, fontScale: 1 } });
    await render(<DevicesView {...props()} />);
    for (const device of devices) {
      expect(layoutOf(device.id)).toMatchObject({ flexDirection: 'row' });
      expect(layoutOf(device.id).flexWrap).toBeUndefined();
    }
  });
});

describe('signing a device out without a swipe (S9)', () => {
  it('every other device has a visible Sign out button, its own element; this device has none', async () => {
    const onRevoke = jest.fn(async () => true);
    await render(<DevicesView {...props({ onRevoke })} />);
    expect(screen.queryByRole('button', { name: 'Sign out iPhone' })).toBeNull();
    const button = screen.getByRole('button', { name: 'Sign out Pixel 9' });
    for (let node = button.parent; node; node = node.parent) expect(node.props.accessible).not.toBe(true);
    await fireEvent.press(button);
    // The route's destructive confirmation runs inside onRevoke, as for a swipe.
    expect(onRevoke).toHaveBeenCalledWith(devices[1]);
    expect(screen.getByRole('button', { name: 'Sign out iPad' })).toBeTruthy();
    expect(screen.queryByText(/Swipe left/)).toBeNull();
  });

  it('the swipe stays as the shortcut, and as the screen-reader action on the row', async () => {
    await render(<DevicesView {...props()} />);
    expect(screen.getByLabelText(/^Pixel 9, /).props.accessibilityActions).toEqual([{ name: 'delete', label: 'Sign out' }]);
  });
});
