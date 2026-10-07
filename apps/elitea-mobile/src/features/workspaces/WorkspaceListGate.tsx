import { useEffect, useState, type ReactNode } from 'react';
import { AppState, Pressable, StyleSheet, View } from 'react-native';

import { useDesign } from '@/design/DesignProvider';
import { deviceAuth, type DeviceAuth } from '@/security/lock/deviceAuth';
import type { WorkspaceSummary } from '@/state/workspaceList';
import { Icon, pressedOverlay, ripple, Text, uiPlatform } from '@/ui';

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
  const design = useDesign();
  const { colors, radius } = design;
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
      android_ripple={ripple(design)}
      style={({ pressed }) => [
        styles.button,
        {
          backgroundColor: pressed && uiPlatform === 'ios' ? pressedOverlay(design) : colors.surface,
          borderColor: colors.separator,
          borderRadius: radius.listRow,
        },
      ]}>
      <Icon name="lock" size={22} tone="accent" />
      <View style={styles.text}>
        <Text variant="headline">Show your workspaces on this device</Text>
        <Text variant="footnote" tone="secondary">
          One of them requires the device lock.
        </Text>
      </View>
    </Pressable>
  );
}

const styles = StyleSheet.create({
  button: {
    flexDirection: 'row',
    alignItems: 'center',
    gap: 12,
    borderWidth: StyleSheet.hairlineWidth,
    padding: 16,
    marginTop: 12,
    minHeight: 60,
    borderCurve: 'continuous',
  },
  text: { flex: 1, gap: 2 },
});
