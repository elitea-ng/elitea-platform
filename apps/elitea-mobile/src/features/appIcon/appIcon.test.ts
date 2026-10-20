import {
  __resetAppIconForTests,
  getAppIcon,
  offerWorkspaceIconOnce,
  setAppIcon,
  shouldOfferWorkspaceIcon,
  WORKSPACE_ICON_NAME,
  workspaceIconOfferText,
} from '@/features/appIcon/appIcon';
import { DEVICE_KEYS, deleteDeviceItem } from '@/security/secureStorage';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());

function device(current: string | null = null) {
  let icon = current;
  return {
    supportsAlternateIcons: true,
    setAlternateAppIcon: jest.fn(async (name: string | null) => {
      icon = name;
      return name;
    }),
    getAppIconName: jest.fn(() => icon),
  };
}

const SELF_HOSTED = { deployment_kind: 'self_hosted' as const };
const SAAS = { deployment_kind: 'saas' as const };

beforeEach(async () => {
  __resetAppIconForTests();
  await deleteDeviceItem(DEVICE_KEYS.appIconOffered);
});

describe('the workspace app icon (design-system §3.6)', () => {
  it('is offered only for a self-hosted workspace, on a device that can switch, from the default icon, once', () => {
    const ok = { supported: true, current: 'default' as const, offered: false };
    expect(shouldOfferWorkspaceIcon(SELF_HOSTED, ok)).toBe(true);
    expect(shouldOfferWorkspaceIcon(SAAS, ok)).toBe(false);
    expect(shouldOfferWorkspaceIcon(SELF_HOSTED, { ...ok, supported: false })).toBe(false);
    expect(shouldOfferWorkspaceIcon(SELF_HOSTED, { ...ok, current: 'workspace' })).toBe(false);
    expect(shouldOfferWorkspaceIcon(SELF_HOSTED, { ...ok, offered: true })).toBe(false);
  });

  it('asks once per device; accepting switches to Zefir Night, and Settings switches back', async () => {
    const module = device();
    const ask = jest.fn(async () => true);
    await expect(offerWorkspaceIconOnce(SELF_HOSTED, { ask, module })).resolves.toBe(true);
    expect(module.setAlternateAppIcon).toHaveBeenCalledWith(WORKSPACE_ICON_NAME);
    expect(getAppIcon(module)).toBe('workspace');

    // Connecting another self-hosted workspace never asks again.
    await setAppIcon('default', module);
    await expect(offerWorkspaceIconOnce(SELF_HOSTED, { ask, module })).resolves.toBe(false);
    expect(ask).toHaveBeenCalledTimes(1);
    expect(module.setAlternateAppIcon).toHaveBeenLastCalledWith(null);
    expect(getAppIcon(module)).toBe('default');
  });

  it('declining keeps the icon and still counts as the one offer', async () => {
    const module = device();
    const ask = jest.fn(async () => false);
    await expect(offerWorkspaceIconOnce(SELF_HOSTED, { ask, module })).resolves.toBe(false);
    expect(module.setAlternateAppIcon).not.toHaveBeenCalled();
    await offerWorkspaceIconOnce(SELF_HOSTED, { ask, module });
    expect(ask).toHaveBeenCalledTimes(1);
  });

  it('never asks for a hosted (SaaS) workspace', async () => {
    const ask = jest.fn(async () => true);
    await expect(offerWorkspaceIconOnce(SAAS, { ask, module: device() })).resolves.toBe(false);
    expect(ask).not.toHaveBeenCalled();
  });

  it('says on Android that the app may close while the icon changes', () => {
    expect(workspaceIconOfferText('android').message).toMatch(/may close/);
    expect(workspaceIconOfferText('ios').message).not.toMatch(/may close/);
  });
});
