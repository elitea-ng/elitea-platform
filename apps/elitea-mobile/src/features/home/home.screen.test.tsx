import { fireEvent, render, screen } from '@testing-library/react-native';
import type { ReactElement } from 'react';
import { StyleSheet } from 'react-native';

import { DesignProvider } from '@/design/DesignProvider';
import { firstNameOf, HOME_TITLE_AFTER, HomeView, homeHeaderOptions, homeScrollState, type HomeViewProps } from '@/features/home/HomeView';
import { GALLERY_BRANDS } from '@/features/designGallery/DesignGallery';
import { markTint, scaledMarkSize, WorkspaceMark } from '@/features/workspaces/WorkspaceMark';
import { workspacePillLines } from '@/features/workspaces/WorkspacePill';
import { resolveDesign } from '@/design/resolve';

jest.mock('expo-router', () => ({ router: { push: jest.fn(), navigate: jest.fn() }, Stack: { Screen: () => null } }));
jest.mock('@/theme/brandStore', () => ({ useBrandTheme: () => null }));
jest.mock('@/theme/brandFonts', () => ({ useBrandFontFamily: () => undefined }));

const ORIGIN = 'https://acme.elitea.ai';

function props(over: Partial<HomeViewProps> = {}): HomeViewProps {
  return {
    firstName: 'Alex',
    workspace: { origin: ORIGIN, name: 'Acme Corp AI', logoMark: null, brand: null },
    project: 'Zefir Team',
    onWorkspace: jest.fn(),
    offline: false,
    recents: [
      { id: 1, title: 'Zefir launch planning', age: '12m' },
      { id: 2, title: '', age: '3h' },
    ],
    recentsFailed: false,
    onRetry: jest.fn(),
    onOpenChat: jest.fn(),
    onSeeAll: jest.fn(),
    onNewChat: jest.fn(),
    onDiscover: jest.fn(),
    onVoice: jest.fn(),
    onAttachPhoto: jest.fn(),
    onAttachFile: jest.fn(),
    refreshing: false,
    onRefresh: jest.fn(),
    ...over,
  };
}

describe('Home (docs/screens.md §2)', () => {
  it('greets the user by first name in one heading, and keeps the workspace host and project on screen', async () => {
    await render(<HomeView {...props()} />);
    expect(screen.getByTestId('home-greeting')).toHaveTextContent('Hello, Alex\nWhat can I help with?');
    expect(screen.getByText('acme.elitea.ai')).toBeTruthy();
    expect(screen.getByText('Acme Corp AI · Zefir Team')).toBeTruthy();
    expect(screen.getByRole('button', { name: /Workspace Acme Corp AI, acme\.elitea\.ai, project Zefir Team/ })).toBeTruthy();
  });

  it('never puts the project where the workspace name goes on an unbranded workspace (EH-3)', async () => {
    await render(<HomeView {...props({ workspace: { origin: ORIGIN, name: null, logoMark: null, brand: null } })} />);
    expect(screen.getByText('acme.elitea.ai · Zefir Team')).toBeTruthy();
    expect(screen.queryByText('Zefir Team')).toBeNull();
    expect(workspacePillLines('acme.elitea.ai', 'Acme Corp AI', 'Zefir Team')).toEqual({ title: 'Acme Corp AI · Zefir Team', caption: 'acme.elitea.ai' });
    expect(workspacePillLines('acme.elitea.ai', null, 'Zefir Team')).toEqual({ title: 'acme.elitea.ai · Zefir Team', caption: null });
    expect(workspacePillLines('acme.elitea.ai', null, null)).toEqual({ title: null, caption: 'acme.elitea.ai' });
    expect(workspacePillLines('acme.elitea.ai', 'Acme Corp AI', null)).toEqual({ title: 'Acme Corp AI', caption: 'acme.elitea.ai' });
  });

  it('offers New chat, Discover, Dictate and Attach as cards, each one button with a hint', async () => {
    const p = props();
    await render(<HomeView {...p} />);
    for (const [id, handler] of [
      ['new', p.onNewChat],
      ['discover', p.onDiscover],
      ['voice', p.onVoice],
      ['attach', p.onAttachFile],
    ] as const) {
      const card = screen.getByTestId(`home-card-${id}`);
      expect(card.props.accessibilityHint).toBeTruthy();
      await fireEvent.press(card);
      expect(handler).toHaveBeenCalled();
    }
  });

  it('leads the chips with "Choose an agent" as an action, not a selected filter (F3)', async () => {
    const p = props();
    await render(<HomeView {...p} />);
    const chip = screen.getByRole('button', { name: 'Choose an agent' });
    expect(chip.props.accessibilityState?.selected).toBe(false);
    await fireEvent.press(chip);
    expect(p.onNewChat).toHaveBeenCalled();
  });

  it('hides Dictate where the device cannot dictate on-device', async () => {
    await render(<HomeView {...props({ onVoice: null })} />);
    expect(screen.queryByTestId('home-card-voice')).toBeNull();
    expect(screen.queryByRole('button', { name: 'Dictate' })).toBeNull();
  });

  it('lists recent chats (untitled ones named), opens one, and offers See all', async () => {
    const p = props();
    await render(<HomeView {...p} />);
    expect(screen.getByRole('button', { name: 'Untitled conversation, 3h' })).toBeTruthy();
    await fireEvent.press(screen.getByTestId('home-recent-1'));
    expect(p.onOpenChat).toHaveBeenCalledWith(1);
    await fireEvent.press(screen.getByTestId('home-see-all'));
    expect(p.onSeeAll).toHaveBeenCalled();
  });

  it('with no chats yet shows the mascot empty state, without a second New chat under the tab bar (and no See all)', async () => {
    await render(<HomeView {...props({ recents: [] })} />);
    expect(screen.getByRole('header', { name: 'Ask me anything' })).toBeTruthy();
    // Only the New chat card: the empty state has no button of its own (EH-4).
    expect(screen.getAllByRole('button', { name: 'New chat' })).toHaveLength(1);
    expect(screen.queryByTestId('home-see-all')).toBeNull();
  });

  it('with no active project explains why instead of offering cards that dead-end (F1)', async () => {
    await render(<HomeView {...props({ noProject: true, recents: [] })} />);
    expect(screen.getByRole('header', { name: 'No active project' })).toBeTruthy();
    expect(screen.queryByTestId('home-card-new')).toBeNull();
    expect(screen.queryByRole('button', { name: 'Choose an agent' })).toBeNull();
    expect(screen.queryByRole('header', { name: 'Recent chats' })).toBeNull();
    // The workspace pill stays: it is the way to another project or workspace.
    expect(screen.getByText('acme.elitea.ai')).toBeTruthy();
  });

  it('says it is offline with an icon and text, not colour alone', async () => {
    await render(<HomeView {...props({ offline: true })} />);
    expect(screen.getByText(/Offline\. Showing what was saved/)).toBeTruthy();
  });

  it('reads the first name from a profile name', () => {
    expect(firstNameOf('Alex Morgan')).toBe('Alex');
    expect(firstNameOf('  ')).toBeNull();
    expect(firstNameOf(null)).toBeNull();
  });
});

describe('Home navigation bar (§4.1)', () => {
  const design = resolveDesign({ scheme: 'light', platform: 'ios' });
  const base = { userName: 'Alex Morgan', avatarUri: null, unseen: 3, onBell: jest.fn(), onAvatar: jest.fn() };

  it('shows the inline "Home" only once the greeting scrolled away', () => {
    expect(homeHeaderOptions(design, { ...base, collapsed: false }, 'ios').headerTitle).toBe('');
    expect(homeHeaderOptions(design, { ...base, collapsed: true }, 'ios').headerTitle).toBe('Home');
  });

  it('gives the bar its material as soon as the content moves, the title only once it has collapsed', () => {
    expect(homeHeaderOptions(design, { ...base, collapsed: false }, 'ios').headerBlurEffect).toBeUndefined();
    expect(homeHeaderOptions(design, { ...base, collapsed: false, scrolled: true }, 'ios').headerBlurEffect).toBe('systemChromeMaterial');
    expect(homeHeaderOptions(design, { ...base, collapsed: false, scrolled: true }, 'ios').headerTitle).toBe('');
    expect(homeHeaderOptions(design, { ...base, collapsed: false, scrolled: true }, 'android').headerStyle).toEqual({ backgroundColor: design.colors.background });
  });

  it('measures the scroll from the resting offset under the automatic iOS inset (EH-1)', () => {
    // iOS, automatic adjustment: at rest y = -(status bar + bar) and the event's contentInset is the manual 0.
    const rest = 106;
    const at = (y: number) => homeScrollState({ contentOffset: { x: 0, y }, contentInset: { top: 0, left: 0, bottom: 0, right: 0 } }, rest);
    expect(at(-rest)).toBe('rest');
    expect(at(-rest - 60)).toBe('rest'); // pulled down to refresh
    expect(at(-rest + 8)).toBe('scrolled');
    expect(at(-rest + HOME_TITLE_AFTER + 1)).toBe('collapsed');
    // Before the fix the test was y + contentInset.top > 96, which needed ~200 pt of scroll (y > 96).
    expect(at(0)).toBe('collapsed');
    expect(at(-20)).toBe('scrolled');
    // Android: the content is padded instead, so rest is y = 0.
    expect(homeScrollState({ contentOffset: { x: 0, y: 0 } })).toBe('rest');
    expect(homeScrollState({ contentOffset: { x: 0, y: HOME_TITLE_AFTER + 1 } })).toBe('collapsed');
  });

  it('feeds the header state from the scroll view', async () => {
    const onScrollState = jest.fn();
    await render(<HomeView {...props({ scrollRestInset: 106, onScrollState })} />);
    await fireEvent.scroll(screen.getByTestId('home'), { nativeEvent: { contentOffset: { x: 0, y: 0 }, contentInset: { top: 0, left: 0, bottom: 0, right: 0 } } });
    expect(onScrollState).toHaveBeenLastCalledWith('collapsed');
    await fireEvent.scroll(screen.getByTestId('home'), { nativeEvent: { contentOffset: { x: 0, y: -106 }, contentInset: { top: 0, left: 0, bottom: 0, right: 0 } } });
    expect(onScrollState).toHaveBeenLastCalledWith('rest');
  });

  it('has the bell with the unread count (native badge on iOS 26) and the avatar opening Settings', async () => {
    const options = homeHeaderOptions(design, { ...base, collapsed: false }, 'android');
    await render(options.headerRight!({ canGoBack: false }) as ReactElement);
    expect(screen.getByRole('button', { name: 'Notifications, 3 unread' })).toBeTruthy();
    await render(options.headerLeft!({ canGoBack: false }) as ReactElement);
    await fireEvent.press(screen.getByTestId('home-avatar'));
    expect(base.onAvatar).toHaveBeenCalled();
    expect(screen.getByRole('button', { name: 'Alex Morgan. Settings' })).toBeTruthy();
  });
});

describe('each workspace in its own hue (second workspace hue)', () => {
  it('tints a workspace monogram with THAT workspace\'s corrected accent, neutral when unbranded', () => {
    const magenta = markTint(GALLERY_BRANDS.magenta, 'light');
    const green = markTint(GALLERY_BRANDS.green, 'light');
    expect(magenta).not.toBeNull();
    expect(green).not.toBeNull();
    expect(magenta).not.toEqual(green);
    expect(markTint(null, 'light')).toBeNull();
    expect(markTint({ productDefault: true, lightAccent: '#14B8A6' }, 'light')).toBeNull();
  });

  it('scales an inline mark with the text beside it, up to that role\'s cap, and outlines the neutral tile (EH-8)', async () => {
    const footnoteCap = resolveDesign({ scheme: 'light', platform: 'ios' }).typography.footnote.maxFontSizeMultiplier;
    expect(scaledMarkSize(20, 1, footnoteCap)).toBe(20);
    expect(scaledMarkSize(20, 1.5, footnoteCap)).toBe(30);
    expect(scaledMarkSize(20, 3.1, footnoteCap)).toBe(Math.round(20 * footnoteCap));
    expect(scaledMarkSize(20, 0.8, footnoteCap)).toBe(20);
    await render(<WorkspaceMark origin={ORIGIN} image={null} brand={null} size={20} />);
    const design = resolveDesign({ scheme: 'light', platform: 'ios' });
    const tile = screen.getByTestId('workspace-mark-tile', { includeHiddenElements: true });
    expect(StyleSheet.flatten(tile.props.style)).toMatchObject({ backgroundColor: design.colors.surface, borderColor: design.colors.separatorStrong });
  });

  it('renders under a branded provider without changing the mark of another workspace', async () => {
    await render(
      <DesignProvider brand={GALLERY_BRANDS.magenta}>
        <HomeView {...props({ workspace: { origin: ORIGIN, name: null, logoMark: null, brand: GALLERY_BRANDS.green } })} />
      </DesignProvider>,
    );
    // The decorative monogram (hidden from screen readers: the host is written next to it).
    expect(screen.getByText('A', { includeHiddenElements: true })).toBeTruthy();
  });
});
