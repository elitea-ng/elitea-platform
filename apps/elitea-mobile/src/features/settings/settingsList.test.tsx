import { fireEvent, render, screen } from '@testing-library/react-native';

import { SettingsList, type SettingsListProps } from '@/features/settings/SettingsList';

jest.mock('@/theme/brandStore', () => ({ useBrandTheme: () => null }));
jest.mock('@/theme/brandFonts', () => ({ useBrandFontFamily: () => undefined }));
jest.mock('expo-clipboard', () => ({ setStringAsync: jest.fn(async () => true) }));

const base: SettingsListProps = {
  serverOrigin: 'https://acme.elitea.ai',
  logoMark: null,
  projectId: 42,
  workspaceCount: 1,
  developerSignIn: false,
  scheme: 'system',
  onSchemeChange: () => {},
  lockText: 'When the app starts',
  screenshotsAllowed: false,
  securityFooter: 'Security settings: set by the workspace',
  onSwitchWorkspace: () => {},
  onPrivacy: () => {},
  onSupport: () => {},
  onLogOut: () => {},
};

it('an account row names the person and the workspace they are signed in to', async () => {
  await render(<SettingsList {...base} userName="Alex Morgan" />);
  expect(screen.getByLabelText('Signed in as Alex Morgan to acme.elitea.ai')).toBeTruthy();
});

it('a branded workspace shows its name with the host under it, never the name alone', async () => {
  await render(<SettingsList {...base} workspaceName="Acme Corp AI" />);
  expect(screen.getByText('Acme Corp AI')).toBeTruthy();
  expect(screen.getByText('acme.elitea.ai')).toBeTruthy();
});

it('Signed-in devices and About are rows only when the screen can open them', async () => {
  const onDevices = jest.fn();
  const onAbout = jest.fn();
  const view = await render(<SettingsList {...base} />);
  expect(screen.queryByText('Signed-in devices')).toBeNull();
  expect(screen.queryByText('About Agent Zefir')).toBeNull();
  await view.rerender(<SettingsList {...base} onDevices={onDevices} onAbout={onAbout} appVersion="0.1.0" />);
  await fireEvent.press(screen.getByText('Signed-in devices'));
  await fireEvent.press(screen.getByRole('button', { name: 'About Agent Zefir, 0.1.0' }));
  expect(onDevices).toHaveBeenCalled();
  expect(onAbout).toHaveBeenCalled();
});
