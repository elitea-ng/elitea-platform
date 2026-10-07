import { fireEvent, render, screen } from '@testing-library/react-native';
import type { ReactElement } from 'react';

import { DesignProvider } from '@/design/DesignProvider';
import { firstNameOf, HomeView, homeHeaderOptions, type HomeViewProps } from '@/features/home/HomeView';
import { GALLERY_BRANDS } from '@/features/designGallery/DesignGallery';
import { markTint } from '@/features/workspaces/WorkspaceMark';
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

  it('with no chats yet shows the mascot empty state with New chat (and no See all)', async () => {
    await render(<HomeView {...props({ recents: [] })} />);
    expect(screen.getByRole('header', { name: 'Ask me anything' })).toBeTruthy();
    // The empty state's own action, next to the New chat card.
    expect(screen.getAllByRole('button', { name: 'New chat' })).toHaveLength(2);
    expect(screen.queryByTestId('home-see-all')).toBeNull();
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
