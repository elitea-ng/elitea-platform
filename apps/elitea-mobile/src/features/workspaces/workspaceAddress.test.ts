import * as Clipboard from 'expo-clipboard';
import * as Haptics from 'expo-haptics';

import { copyWorkspaceAddress, workspaceAddressTitle } from '@/features/workspaces/workspaceAddress';

jest.mock('expo-clipboard', () => ({ setStringAsync: jest.fn(async () => true) }));

// F6: Settings showed displayHost(origin), which strips http:// too, so a plain-http dev
// workspace read exactly like a secure one, and the address could no longer be copied.
describe('workspace address in Settings (F6)', () => {
  it('shows the host for https and keeps any other scheme', () => {
    expect(workspaceAddressTitle('https://ai.example.com')).toBe('ai.example.com');
    expect(workspaceAddressTitle('https://ai.example.com:8443')).toBe('ai.example.com:8443');
    expect(workspaceAddressTitle('http://10.0.0.5')).toBe('http://10.0.0.5');
    expect(workspaceAddressTitle('http://localhost:8081')).toBe('http://localhost:8081');
  });

  it('copies the full origin, scheme included, with the copy haptic', async () => {
    await copyWorkspaceAddress('http://10.0.0.5');
    expect(Clipboard.setStringAsync).toHaveBeenCalledWith('http://10.0.0.5');
    expect(Haptics.notificationAsync).toHaveBeenCalledWith('success');
  });
});
