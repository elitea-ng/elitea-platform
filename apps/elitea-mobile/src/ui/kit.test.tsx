import { fireEvent, render, screen } from '@testing-library/react-native';
import * as Haptics from 'expo-haptics';
import type { ReactElement } from 'react';
import { StyleSheet, type Insets } from 'react-native';

import { DesignProvider } from '@/design/DesignProvider';
import { resolveDesign } from '@/design/resolve';
import { colors as tokenColors } from '@/design/tokens';
import { contrastRatio, parseColor } from '@/theme/color';

import {
  ActionCard,
  Avatar,
  AVATAR_TONES,
  Badge,
  badgeText,
  Button,
  buttonHeight,
  Chip,
  EmptyState,
  glassFill,
  glassMode,
  gradientPoints,
  headerActionOptions,
  hitSlopFor,
  hitTarget,
  ICONS,
  IconButton,
  initialsOf,
  layoutMargin,
  ListRow,
  ListSection,
  sectionTitle,
  SegmentedControl,
  selectedSegmentStyle,
  sheetOptions,
  symbolFor,
  toneIndex,
  withAlpha,
} from './index';

import { ProjectChip } from '@/features/projects/ProjectChip';

declare const __dirname: string;
const fs = require('fs') as { readFileSync(path: string, encoding: 'utf8'): string };

jest.mock('@/theme/brandStore', () => ({ useBrandTheme: () => null }));
jest.mock('expo-router', () => ({ router: { push: jest.fn() } }));
jest.mock('@/features/projects/useProjectSelection', () => ({ useProjectSelection: () => ({ project: null, projectId: null }) }));
jest.mock('@/state/selfIdentity', () => ({ usePersonalProjectId: () => null }));
jest.mock('@/theme/brandFonts', () => ({ useBrandFontFamily: () => undefined }));

const light = resolveDesign({ scheme: 'light', platform: 'ios' });
const dark = resolveDesign({ scheme: 'dark', platform: 'ios' });

function inScheme(ui: ReactElement, scheme: 'light' | 'dark' = 'light') {
  return render(<DesignProvider scheme={scheme}>{ui}</DesignProvider>);
}

/** The tap area a pressable gets: its own frame grown by its hitSlop. */
function tapArea(element: { props: { style?: unknown; hitSlop?: Insets | number }; children?: unknown[] }): { width: number; height: number } {
  const raw = element.props.style;
  let style = StyleSheet.flatten(typeof raw === 'function' ? raw({ pressed: false }) : raw) ?? {};
  // A row draws its minimum height on its content view, which fills the pressable.
  const child = (element.children?.[0] ?? null) as { props?: { style?: unknown } } | null;
  if (!style.height && !style.minHeight && child?.props) style = { ...StyleSheet.flatten(child.props.style as never), ...style };
  const slop = element.props.hitSlop;
  const s: Insets = typeof slop === 'number' ? { top: slop, bottom: slop, left: slop, right: slop } : (slop ?? {});
  const height = Number(style.height ?? style.minHeight ?? 0) + (s.top ?? 0) + (s.bottom ?? 0);
  const width = Number(style.width ?? style.minWidth ?? Infinity) + (s.left ?? 0) + (s.right ?? 0);
  return { width, height };
}

describe('touch targets (audit X12, §4.13)', () => {
  it('are 44 pt on iOS and 48 dp on Android', () => {
    expect(hitTarget('ios')).toBe(44);
    expect(hitTarget('android')).toBe(48);
  });

  it('hitSlop grows a small visual to the target and leaves a big one alone', () => {
    expect(hitSlopFor(36, 'ios')).toEqual({ top: 4, bottom: 4, left: 4, right: 4 });
    expect(hitSlopFor(36, 'android')).toEqual({ top: 6, bottom: 6, left: 6, right: 6 });
    expect(hitSlopFor(20, 'ios')).toEqual({ top: 12, bottom: 12, left: 12, right: 12 });
    expect(hitSlopFor(50, 'ios')).toBeUndefined();
  });

  it('buttons are 50 pt / 48 dp; a small one keeps the target with hitSlop', () => {
    expect(buttonHeight('regular', 'ios')).toBe(50);
    expect(buttonHeight('regular', 'android')).toBe(48);
    expect(buttonHeight('small', 'ios') + 2 * (hitSlopFor(36, 'ios')?.top ?? 0)).toBeGreaterThanOrEqual(44);
    expect(buttonHeight('small', 'android') + 2 * (hitSlopFor(36, 'android')?.top ?? 0)).toBeGreaterThanOrEqual(48);
  });

  it.each([
    ['Button', <Button key="b" title="Continue" onPress={() => {}} />],
    ['small Button', <Button key="s" title="Retry" size="small" onPress={() => {}} />],
    ['IconButton', <IconButton key="i" icon="bell" accessibilityLabel="Notifications" onPress={() => {}} />],
    ['plain IconButton', <IconButton key="p" icon="close" variant="plain" size={20} accessibilityLabel="Close" onPress={() => {}} />],
    ['Chip', <Chip key="c" label="Research" onPress={() => {}} />],
    ['ListRow', <ListRow key="l" title="Memory" onPress={() => {}} />],
    ['ProjectChip', <ProjectChip key="pc" label="Marketing" />],
  ])('%s takes at least a 44 pt tap', async (_name, ui) => {
    await inScheme(ui);
    const [button] = screen.getAllByRole('button');
    const area = tapArea(button as never);
    expect(area.height).toBeGreaterThanOrEqual(44);
    expect(area.width).toBeGreaterThanOrEqual(44);
  });
});

describe('layout margin (D7: content under a large title shares its leading edge)', () => {
  it.each([
    ['ios', 375, 16],
    ['ios', 393, 16],
    ['ios', 440, 20],
    ['ios', 820, 20],
    ['android', 440, 16],
  ] as const)('%s at %p pt wide is %p', (on, width, margin) => {
    expect(layoutMargin(width, on)).toBe(margin);
  });
});

describe('Button', () => {
  it('speaks its title, fires the haptic, and reads as busy while loading', async () => {
    const onPress = jest.fn();
    await inScheme(<Button title="Send" haptic="send" onPress={onPress} />);
    await fireEvent.press(screen.getByRole('button', { name: 'Send' }));
    expect(onPress).toHaveBeenCalledTimes(1);
    expect(Haptics.impactAsync).toHaveBeenCalledWith('light');
    await inScheme(<Button title="Signing in" loading onPress={onPress} />);
    expect(screen.getByRole('button', { name: 'Signing in' }).props.accessibilityState).toEqual({ disabled: true, busy: true });
  });
});

describe('Chip', () => {
  it('announces "selected" only for a filter chip, not for a filled action chip', async () => {
    const { colors } = light;
    await inScheme(<Chip label="Research" selected onPress={() => {}} />);
    expect(screen.getByRole('button', { name: 'Research' }).props.accessibilityState).toEqual({ selected: true, disabled: false });
    await inScheme(<Chip label="Choose an agent" filled menu onPress={() => {}} />);
    const action = screen.getByRole('button', { name: 'Choose an agent' });
    expect(action.props.accessibilityState).toEqual({ selected: false, disabled: false });
    expect(StyleSheet.flatten(typeof action.props.style === 'function' ? action.props.style({ pressed: false }) : action.props.style)).toMatchObject({ backgroundColor: colors.chipSelected });
  });
});

describe('Icon', () => {
  it('pairs every semantic name with an SF Symbol and a Material Symbol that exists in the font', () => {
    const symbols = JSON.parse(fs.readFileSync(`${__dirname}/../../node_modules/expo-symbols/src/android/symbols.json`, 'utf8')) as Record<string, unknown>;
    for (const [name, [sf, md]] of Object.entries(ICONS)) {
      expect([name, typeof sf === 'string' && sf.length > 0]).toEqual([name, true]);
      expect([name, md in symbols]).toEqual([name, true]);
    }
  });

  it('picks the platform symbol, filled when selected on iOS only', () => {
    expect(symbolFor('bell', 'ios')).toBe('bell');
    expect(symbolFor('bell', 'ios', true)).toBe('bell.fill');
    expect(symbolFor('bell', 'android', true)).toBe('notifications');
    expect(symbolFor('send', 'ios', true)).toBe('arrow.up');
  });

  it('an icon-only button draws the symbol for both platforms and is labelled', async () => {
    await inScheme(<IconButton icon="newChat" accessibilityLabel="New chat" onPress={() => {}} />);
    expect(screen.getByRole('button', { name: 'New chat' })).toBeTruthy();
    expect(JSON.stringify(screen.toJSON())).toContain('"symbolIos":"square.and.pencil","symbolAndroid":"edit_square"');
  });
});

describe('Badge', () => {
  it('counts to 99, then 99+, and hides at zero', async () => {
    expect(badgeText(3)).toBe('3');
    expect(badgeText(100)).toBe('99+');
    await inScheme(<Badge count={0} />);
    expect(screen.queryByText('0')).toBeNull();
  });

  it('sits inside the button frame (never a negative offset a glass capsule clips, audit 2.8)', async () => {
    await inScheme(<IconButton icon="bell" badge={4} accessibilityLabel="Notifications, 4 unread" onPress={() => {}} />);
    let slot = screen.getByText('4', { includeHiddenElements: true }).parent;
    while (slot && slot.props.pointerEvents !== 'none') slot = slot.parent;
    const style = StyleSheet.flatten(slot?.props.style);
    expect(style.top).toBe(0);
    expect(style.end).toBe(0);
  });
});

describe('navigation bar actions (X5, X6)', () => {
  const actions = [
    { id: 'notifications', icon: 'bell' as const, label: 'Notifications', accessibilityLabel: 'Notifications, 2 unread', badge: 2, onPress: jest.fn() },
    { id: 'new', icon: 'newChat' as const, label: 'New chat', onPress: jest.fn() },
  ];

  it('iOS 26: native SF Symbol bar buttons with the system badge', () => {
    const options = headerActionOptions(actions, light, 'ios', 26);
    expect(options.headerRight).toBeUndefined();
    const items = options.unstable_headerRightItems?.({}) ?? [];
    expect(items).toEqual([
      expect.objectContaining({
        type: 'button',
        icon: { type: 'sfSymbol', name: 'bell' },
        accessibilityLabel: 'Notifications, 2 unread',
        badge: { value: '2', style: { backgroundColor: light.colors.badge, color: light.colors.badgeText } },
      }),
      expect.objectContaining({ type: 'button', icon: { type: 'sfSymbol', name: 'square.and.pencil' }, badge: undefined }),
    ]);
  });

  it.each([
    ['Android', 'android', 0],
    ['iOS 18', 'ios', 18],
  ] as const)('%s: kit icon buttons with the badge in their frame', async (_name, platform, major) => {
    const options = headerActionOptions(actions, light, platform, major);
    expect(options.unstable_headerRightItems).toBeUndefined();
    await inScheme(<>{options.headerRight?.({})}</>);
    expect(screen.getByRole('button', { name: 'Notifications, 2 unread' })).toBeTruthy();
    expect(screen.getByRole('button', { name: 'New chat' })).toBeTruthy();
    expect(screen.getByText('2', { includeHiddenElements: true })).toBeTruthy();
  });

  it('iOS 26: the native items forward onPress and disabled, and say "in progress" while busy (F3)', () => {
    const onPress = jest.fn();
    const busy = [{ id: 'export', icon: 'share' as const, label: 'Export conversation', disabled: true, busy: true, onPress }];
    const [item] = headerActionOptions(busy, light, 'ios', 26).unstable_headerRightItems?.({}) ?? [];
    expect(item).toMatchObject({ type: 'button', disabled: true, accessibilityLabel: 'Export conversation, in progress' });
    (item as { onPress: () => void }).onPress();
    expect(onPress).toHaveBeenCalledTimes(1);
    const [idle] = headerActionOptions([{ ...busy[0], disabled: false, busy: false }], light, 'ios', 26).unstable_headerRightItems?.({}) ?? [];
    expect(idle).toMatchObject({ disabled: false, accessibilityLabel: 'Export conversation' });
  });

  it('Android / iOS 18: a busy action reads as busy', async () => {
    const options = headerActionOptions([{ id: 'export', icon: 'share', label: 'Export conversation', disabled: true, busy: true, onPress: () => {} }], light, 'android', 0);
    await inScheme(<>{options.headerRight?.({})}</>);
    expect(screen.getByRole('button', { name: 'Export conversation' }).props.accessibilityState).toMatchObject({ busy: true, disabled: true });
  });

  it('no actions, no header items', () => {
    expect(headerActionOptions([], light, 'ios', 26)).toEqual({});
  });
});

describe('sheets (X9, X10)', () => {
  it('are native form sheets with detents and a grabber', () => {
    expect(sheetOptions(light)).toMatchObject({ presentation: 'formSheet', sheetAllowedDetents: [0.5, 1], sheetGrabberVisible: true });
    expect(sheetOptions(dark, 'fit').sheetAllowedDetents).toBe('fitToContents');
    expect(sheetOptions(dark).contentStyle).toEqual({ backgroundColor: dark.colors.elevated });
  });
});

describe('glass (§1.3)', () => {
  it('is Liquid Glass only on iOS 26+, tonal on Android and older iOS, opaque under Reduce Transparency', () => {
    expect(glassMode('ios', true, true)).toBe('liquid');
    expect(glassMode('ios', false, true)).toBe('tinted');
    expect(glassMode('android', true, true)).toBe('tinted');
    expect(glassMode('ios', true, false)).toBe('opaque');
    expect(glassMode('android', false, false)).toBe('opaque');
  });

  it('draws the fallback colour at α .92, or fully opaque', () => {
    expect(withAlpha('#26222C', 0.5)).toBe('rgba(38, 34, 44, 0.5)');
    expect(glassFill(dark.colors.surfaceGlass, 'tinted')).toBe('rgba(38, 34, 44, 0.92)');
    expect(glassFill(dark.colors.surfaceGlass, 'opaque')).toBe('#26222C');
  });

  it('the gradient follows the 160° token angle', () => {
    expect(gradientPoints(180)).toEqual({ start: { x: 0.5, y: 0 }, end: { x: 0.5, y: 1 } });
    expect(gradientPoints(90)).toEqual({ start: { x: 0, y: 0.5 }, end: { x: 1, y: 0.5 } });
  });
});

describe('lists (X12, X13)', () => {
  it('uppercases iOS section headers only', () => {
    expect(sectionTitle('Security', 'ios')).toBe('SECURITY');
    expect(sectionTitle('Security', 'android')).toBe('Security');
  });

  it('a row that does nothing is not a button; a row that navigates is', async () => {
    await inScheme(
      <ListSection title="Security">
        <ListRow title="Device lock" subtitle="When the app starts" />
        <ListRow title="Memory" onPress={() => {}} />
      </ListSection>,
    );
    expect(screen.queryByRole('button', { name: 'Device lock, When the app starts' })).toBeNull();
    expect(screen.getByLabelText('Device lock, When the app starts')).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Memory' })).toBeTruthy();
  });

  it('mirrors swipe actions as accessibility actions', async () => {
    const onAction = jest.fn();
    await inScheme(
      <ListRow
        title="Q3 planning"
        onPress={() => {}}
        accessibilityActions={[{ name: 'delete', label: 'Delete' }]}
        onAccessibilityAction={(e) => onAction(e.nativeEvent.actionName)}
      />,
    );
    await fireEvent(screen.getByRole('button', { name: 'Q3 planning' }), 'accessibilityAction', { nativeEvent: { actionName: 'delete' } });
    expect(onAction).toHaveBeenCalledWith('delete');
  });
});

describe('segmented control', () => {
  it('is a radio group; choosing fires the selection haptic once', async () => {
    const onChange = jest.fn();
    await inScheme(
      <SegmentedControl
        accessibilityLabel="Appearance"
        value="system"
        onChange={onChange}
        segments={[
          { value: 'system', label: 'System' },
          { value: 'dark', label: 'Dark' },
        ]}
      />,
    );
    expect(screen.getByRole('radio', { name: 'System' }).props.accessibilityState).toMatchObject({ selected: true });
    await fireEvent.press(screen.getByRole('radio', { name: 'System' }));
    expect(onChange).not.toHaveBeenCalled();
    await fireEvent.press(screen.getByRole('radio', { name: 'Dark' }));
    expect(onChange).toHaveBeenCalledWith('dark');
    expect(Haptics.selectionAsync).toHaveBeenCalled();
  });

  // WCAG 1.4.11: on iOS the selected thumb sits on the surfaceSunken track inside a surface card;
  // its edge must reach 3:1 against the track, and in dark mode its fill must not be the card's.
  it.each([
    ['light', light],
    ['dark', dark],
  ] as const)('the iOS %s selected thumb is a 3:1 shape on the track, not the card colour', (scheme, design) => {
    const thumb = selectedSegmentStyle(design.colors, 'ios');
    const track = parseColor(design.colors.surfaceSunken)!;
    expect(thumb.borderWidth).toBeGreaterThanOrEqual(1);
    expect(contrastRatio(parseColor(String(thumb.borderColor))!, track)).toBeGreaterThanOrEqual(3);
    if (scheme === 'dark') expect(thumb.backgroundColor).not.toBe(design.colors.surface);
  });

  it('marks the Android selected segment with the M3 accentSoft fill', () => {
    expect(selectedSegmentStyle(light.colors, 'android')).toEqual({ backgroundColor: light.colors.accentSoft });
  });
});

describe('avatars', () => {
  it('initials of the first two words, and a stable tone per name', () => {
    expect(initialsOf('Alex Smith Jr')).toBe('AS');
    expect(initialsOf('  ')).toBe('?');
    expect(toneIndex('Alex Smith')).toBe(toneIndex(' alex smith '));
  });

  it.each(['light', 'dark'] as const)('every %s initials tone passes 4.5:1', (scheme) => {
    for (const [fill, text] of AVATAR_TONES[scheme]) {
      expect([fill, text, contrastRatio(parseColor(text)!, parseColor(fill)!) >= 4.5]).toEqual([fill, text, true]);
    }
  });

  it('an agent without an icon is the mascot labelled with the AGENT name; others a monogram', async () => {
    await inScheme(
      <>
        <Avatar kind="agent" name="Research" mascot />
        <Avatar kind="agent" name="HR helper" />
      </>,
    );
    expect(screen.getByRole('image', { name: 'Research' })).toBeTruthy();
    expect(screen.getByText('HH')).toBeTruthy();
  });
});

describe('empty state (§4.9)', () => {
  it('has a heading, one action, and a decorative mascot (none for data-loss errors)', async () => {
    const onPress = jest.fn();
    await inScheme(<EmptyState title="Ask me anything" body="Start a chat." action={{ title: 'New chat', onPress }} />);
    expect(screen.getByRole('header', { name: 'Ask me anything' })).toBeTruthy();
    await fireEvent.press(screen.getByRole('button', { name: 'New chat' }));
    expect(onPress).toHaveBeenCalled();
    expect(screen.queryByRole('image')).toBeNull();
    const { toJSON } = await inScheme(<EmptyState mascot="none" title="Something went wrong" />);
    expect(JSON.stringify(toJSON())).not.toContain('mascot');
  });
});

describe('action card (§4.2)', () => {
  it('is one button named by its title; the arrow is a visual only', async () => {
    await inScheme(<ActionCard title="New chat" icon="newChat" accessibilityHint="Starts a new chat" onPress={() => {}} />, 'dark');
    expect(screen.getAllByRole('button')).toHaveLength(1);
    expect(screen.getByRole('button', { name: 'New chat' }).props.accessibilityHint).toBe('Starts a new chat');
  });
});

describe('tokens', () => {
  it('dark is not pure black (audit X15)', () => {
    expect(tokenColors.dark.background).not.toBe('#000000');
  });
});
