import { useEffect, useState, type ReactNode } from 'react';
import { AppState, Pressable, StyleSheet, Text } from 'react-native';

import { Spacing } from '@/constants/theme';
import { usePalette } from '@/constants/use-palette';
import { deviceAuth, type DeviceAuth } from '@/security/lock/deviceAuth';
import type { WorkspaceSummary } from '@/state/workspaceList';
import { ramp } from '@/ui';

/** True when the list names a workspace whose policy requires the device lock. */
export function listNeedsUnlock(workspaces: readonly Pick<WorkspaceSummary, 'requiresLock'>[]): boolean {
  return workspaces.some((w) => w.requiresLock);
}

/**
 * The connect screen's list of workspaces on this device (WS-SEC-4). With no
 * workspace active the lock controller is stopped, so a list naming a
 * workspace whose policy requires the device lock (host, brand, signed-in
 * state) is shown only after the device owner unlocks, and hidden again when
 * the app goes to the background.
 */
export function WorkspaceListGate({
  workspaces,
  children,
  auth = deviceAuth,
}: {
  workspaces: readonly WorkspaceSummary[];
  children: ReactNode;
  auth?: Pick<DeviceAuth, 'authenticate'>;
}) {
  const palette = usePalette();
  const [revealed, setRevealed] = useState(false);
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    const sub = AppState.addEventListener('change', (state) => {
      if (state === 'background') setRevealed(false);
    });
    return () => sub.remove();
  }, []);

  if (revealed || !listNeedsUnlock(workspaces)) return children;
  const unlock = async () => {
    setBusy(true);
    try {
      const outcome = await auth.authenticate('Unlock to see your workspaces');
      if (outcome.ok) setRevealed(true);
    } finally {
      setBusy(false);
    }
  };
  return (
    <Pressable
      accessibilityRole="button"
      accessibilityState={{ busy, disabled: busy }}
      disabled={busy}
      testID="unlock-workspace-list"
      onPress={() => void unlock()}
      style={[styles.button, { borderColor: palette.border }]}>
      <Text style={[styles.text, { color: palette.text }]}>Show your workspaces on this device</Text>
      <Text style={[styles.small, { color: palette.textSecondary }]}>
        One of them requires the device lock.
      </Text>
    </Pressable>
  );
}

const styles = StyleSheet.create({
  button: { borderWidth: 1, borderRadius: 10, padding: Spacing.three, gap: 4, marginTop: Spacing.three },
  text: { ...ramp.headline, fontWeight: '600' },
  small: { ...ramp.footnote },
});
