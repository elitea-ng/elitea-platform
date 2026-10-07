import { useState } from 'react';
import { ActivityIndicator, Linking, Pressable, StyleSheet, Text, View } from 'react-native';

import { StatusScreen } from '@/components/StatusScreen';
import { identity } from '@/config/identity';
import { Spacing } from '@/constants/theme';
import { usePalette } from '@/constants/use-palette';
import { endSession } from '@/security/endSession';
import { getSession, useSession } from '@/state/session';
import { beginAddWorkspace } from '@/state/workspace';
import { useWorkspaceList } from '@/state/workspaceList';
import { refreshWorkspaceInfo } from '@/state/workspaceInfo';
import { ramp } from '@/ui';

/**
 * Asks the workspace again whether this app version is accepted: re-reads the
 * discovery document, whose minimum version clears the update screen when it
 * no longer applies (src/security/policyStore.ts). Resolves true when the
 * screen is gone.
 */
export async function checkVersionAgain(refresh: () => Promise<unknown> = refreshWorkspaceInfo): Promise<boolean> {
  await refresh();
  return getSession().upgradeRequired === null;
}

/**
 * Blocking screen after a 426 from the server, or when the workspace's minimum
 * version is above this app's. Nothing is wiped: updating the app restores
 * access. It clears by itself when the workspace lowers its minimum (re-read
 * on every foreground, after every token response, and on "Check again") or
 * when a signed-in request succeeds again. "Log out & wipe" (this workspace
 * only) and, with several workspaces connected, "Use another workspace" (no
 * wipe) are the other ways out.
 */
export function UpgradeRequired({ minVersion, overlay = false }: { minVersion?: string; overlay?: boolean }) {
  const palette = usePalette();
  const workspaceId = useSession((s) => s.workspaceId);
  const others = useWorkspaceList().filter((w) => w.id !== workspaceId).length;
  const [checking, setChecking] = useState(false);
  const [stillRequired, setStillRequired] = useState(false);
  const body =
    `This workspace needs a newer version of ${identity.displayName}` +
    (minVersion ? ` (${minVersion} or later)` : '') +
    '. Update the app to continue.';
  const onCheckAgain = () => {
    if (checking) return;
    setChecking(true);
    setStillRequired(false);
    void checkVersionAgain().then(
      (cleared) => {
        setChecking(false);
        if (!cleared) setStillRequired(true);
      },
      () => {
        setChecking(false);
        setStillRequired(true);
      },
    );
  };
  const screen = (
    <StatusScreen title={`Update ${identity.displayName}`} body={body}>
      <Pressable
        accessibilityRole="button"
        onPress={() => void Linking.openURL(identity.webUrl)}
        style={[styles.button, { backgroundColor: palette.accent }]}>
        <Text style={[styles.buttonText, { color: palette.onAccent }]}>Get the update</Text>
      </Pressable>
      <Pressable
        accessibilityRole="button"
        accessibilityState={{ busy: checking, disabled: checking }}
        disabled={checking}
        onPress={onCheckAgain}
        style={[styles.button, styles.outline, { borderColor: palette.accent }]}>
        {checking ? (
          <ActivityIndicator color={palette.accent} />
        ) : (
          <Text style={[styles.buttonText, { color: palette.accent }]}>Check again</Text>
        )}
      </Pressable>
      {stillRequired ? (
        <Text accessibilityRole="alert" style={[styles.hint, { color: palette.textSecondary }]}>
          The workspace still needs a newer version.
        </Text>
      ) : null}
      {others > 0 ? (
        <Pressable
          accessibilityRole="button"
          onPress={() => void beginAddWorkspace()}
          style={[styles.button, styles.outline, { borderColor: palette.accent }]}>
          <Text style={[styles.buttonText, { color: palette.accent }]}>Use another workspace</Text>
        </Pressable>
      ) : null}
      <Pressable
        accessibilityRole="button"
        onPress={() => void endSession({ reason: 'user_logout' })}
        style={[styles.button, styles.outline, { borderColor: palette.danger }]}>
        <Text style={[styles.buttonText, { color: palette.danger }]}>Log out & wipe</Text>
      </Pressable>
    </StatusScreen>
  );
  if (!overlay) return screen;
  return (
    <View
      style={[StyleSheet.absoluteFill, { backgroundColor: palette.background }]}
      accessibilityViewIsModal
      importantForAccessibility="yes">
      {screen}
    </View>
  );
}

const styles = StyleSheet.create({
  button: {
    alignSelf: 'stretch',
    borderRadius: 10,
    paddingVertical: Spacing.three - 2,
    alignItems: 'center',
  },
  outline: { borderWidth: 1.5 },
  buttonText: { ...ramp.headline, fontWeight: '600' },
  hint: { ...ramp.subheadline, textAlign: 'center' },
});
