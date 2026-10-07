import { render, screen } from '@testing-library/react-native';

import { ConversationRow } from '@/features/chats/ConversationRow';
import { SettingsList } from '@/features/settings/SettingsList';

jest.mock('@/theme/brandStore', () => ({ useBrandTheme: () => null }));
jest.mock('@/theme/brandFonts', () => ({ useBrandFontFamily: () => undefined }));
jest.mock('expo-clipboard', () => ({ setStringAsync: jest.fn(async () => true) }));

declare const __dirname: string;
const fs = require('fs') as { readFileSync(path: string, encoding: 'utf8'): string };
const path = require('path') as { join(...parts: string[]): string };
const read = (relative: string) => fs.readFileSync(path.join(__dirname, '../../app', relative), 'utf8');

/**
 * D2: the review screenshots came from __DEV__ fixtures that drew their own rows (avatars,
 * previews, a colour-only unread dot, a Face ID switch) — a list no user sees. Each preview now
 * renders the component its real screen renders.
 */
describe('design-tab previews draw the real screens’ components (D2)', () => {
  it.each([
    ['(app)/(tabs)/chats/index.tsx', 'design-tabs/chats/index.tsx', /<ConversationList\b/],
    ['(app)/(tabs)/(home)/index.tsx', 'design-tabs/home/index.tsx', /<HomeView\b/],
    ['(app)/(tabs)/(home)/index.tsx', 'design-tabs/home/index.tsx', /homeHeaderOptions\(/],
    ['(app)/(tabs)/settings/index.tsx', 'design-tabs/settings/index.tsx', /<SettingsList\b/],
  ])('%s and %s both use %p', (real, preview, pattern) => {
    expect(read(real)).toMatch(pattern);
    expect(read(preview)).toMatch(pattern);
  });

  it('the Chats preview draws no avatar, preview line or unread dot of its own', () => {
    expect(read('design-tabs/chats/index.tsx')).not.toMatch(/<Avatar\b|unread|preview:|subtitle=/);
  });

  it('a conversation row reads its title and age', async () => {
    await render(<ConversationRow title="" age="Yesterday" onPress={() => {}} />);
    expect(screen.getByRole('button', { name: 'Untitled conversation, Yesterday' })).toBeTruthy();
  });

  it('the Settings list shows the project by name, the way the tab does, with no Face ID row', async () => {
    await render(
      <SettingsList
        serverOrigin="https://acme.elitea.ai"
        logoMark={null}
        projectId={42}
        projectName="Zefir Team"
        onProject={() => {}}
        workspaceCount={1}
        developerSignIn={false}
        scheme="system"
        onSchemeChange={() => {}}
        lockText="When the app starts"
        screenshotsAllowed={false}
        securityFooter="Security settings: set by the workspace"
        onSwitchWorkspace={() => {}}
        onPrivacy={() => {}}
        onSupport={() => {}}
        onLogOut={() => {}}
      />,
    );
    // The project by its NAME, never its internal id ("Project #90504").
    expect(screen.getByText('Zefir Team')).toBeTruthy();
    expect(screen.queryByText(/#42/)).toBeNull();
    expect(screen.getByText('acme.elitea.ai')).toBeTruthy();
    expect(screen.queryByText('Face ID')).toBeNull();
    expect(screen.queryByText('Memory')).toBeNull();
  });
});
