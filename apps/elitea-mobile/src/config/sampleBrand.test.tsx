/**
 * ADR-0052 decision 11, mobile: built for a brand other than ELITEA, the visible screens
 * name no ELITEA product and draw no ELITEA mark. Every module here loads with the
 * identity of `brand/sample` (read by the real loader, through app.config.ts), the way
 * an `ELITEA_BRAND=sample` build sees it.
 */
import { render, screen } from '@testing-library/react-native';

jest.mock('@/theme/brandStore', () => ({ useBrandTheme: () => null }));
jest.mock('@/theme/brandFonts', () => ({ useBrandFontFamily: () => undefined }));
jest.mock('expo-clipboard', () => ({ setStringAsync: jest.fn(async () => true) }));
jest.mock('react-native-safe-area-context', () => require('react-native-safe-area-context/jest/mock').default);

// The sample brand's config, from app.config.ts with ELITEA_BRAND=sample, installed on
// expo-constants before any module of the app loads (the jest setup filled it with brand/elitea).
// jest.mock factories run first and may only read `mock`-prefixed names.
const mockSampleExtra = (() => {
  const before = process.env.ELITEA_BRAND;
  process.env.ELITEA_BRAND = 'sample';
  try {
    let extra: Record<string, unknown> = {};
    jest.isolateModules(() => {
      const appConfig = require('../../app.config').default as (ctx: { config: Record<string, unknown> }) => { extra: Record<string, unknown> };
      extra = appConfig({ config: {} }).extra;
    });
    return extra;
  } finally {
    if (before === undefined) delete process.env.ELITEA_BRAND;
    else process.env.ELITEA_BRAND = before;
  }
})();
const Constants = require('expo-constants').default as { expoConfig: Record<string, unknown> };
Object.assign(Constants.expoConfig, { extra: mockSampleExtra });

const ELITEA = /elitea/i;

describe('built for brand/sample', () => {
  const sample = mockSampleExtra.identity as { displayName: string; shortName: string; markSvg: string; bundleId: string };

  it('the identity is the sample brand\'s', () => {
    expect(sample.displayName).toBe('Sample Assistant');
    expect(sample.bundleId).toBe('com.example.sample');
    expect(sample.markSvg).not.toMatch(ELITEA);
    expect(require('@/config/identity').identity.displayName).toBe('Sample Assistant');
  });

  it('About shows the sample name and mark, no ELITEA name', async () => {
    const { AboutView } = require('@/features/settings/AboutView') as typeof import('@/features/settings/AboutView');
    const view = await render(<AboutView version="1.0.0" onPrivacy={() => {}} onSupport={() => {}} />);
    expect(screen.getByText('Sample Assistant')).toBeTruthy();
    expect(JSON.stringify(view.toJSON())).not.toMatch(ELITEA);
  });

  it('Settings names About after the sample brand', async () => {
    const { SettingsList } = require('@/features/settings/SettingsList') as typeof import('@/features/settings/SettingsList');
    const view = await render(
      <SettingsList
        serverOrigin="https://acme.example.com"
        logoMark={null}
        projectId={42}
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
        onAbout={() => {}}
        appVersion="1.0.0"
      />,
    );
    expect(screen.getByText('About Sample Assistant')).toBeTruthy();
    expect(JSON.stringify(view.toJSON())).not.toMatch(ELITEA);
  });

  it('the welcome screen and the mark draw the sample brand', async () => {
    const { ConnectWelcome } = require('@/features/entry/ConnectWelcome') as typeof import('@/features/entry/ConnectWelcome');
    const view = await render(
      <ConnectWelcome
        adding={false}
        address=""
        onAddressChange={() => {}}
        error={null}
        managedHint={false}
        busy={false}
        onContinue={() => {}}
        onPrivacy={() => {}}
        onHelp={() => {}}
      />,
    );
    expect(JSON.stringify(view.toJSON())).not.toMatch(ELITEA);
    const { BRAND_MARK_SVG, BRAND_MARK_NAME } = require('@/components/BrandMark') as typeof import('@/components/BrandMark');
    expect(BRAND_MARK_SVG).toBe(sample.markSvg);
    expect(BRAND_MARK_NAME).toBe('Sample');
  });

  it('the widget heading and the lock prompt take the sample name', () => {
    const { signedOutSnapshot } = require('@/features/surfaces/surfaceSnapshot') as typeof import('@/features/surfaces/surfaceSnapshot');
    expect(signedOutSnapshot(0).product).toBe('Sample Assistant');
    expect(signedOutSnapshot(0).openUrl).toBe('sample://surface/home');
  });
});
