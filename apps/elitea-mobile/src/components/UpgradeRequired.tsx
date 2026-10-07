import * as WebBrowser from 'expo-web-browser';
import { useState } from 'react';
import { StyleSheet, View } from 'react-native';

import { StatusScreen } from '@/components/StatusScreen';
import { identity } from '@/config/identity';
import { useDesign } from '@/design/DesignProvider';
import { endSession } from '@/security/endSession';
import { getSession, useSession } from '@/state/session';
import { beginAddWorkspace } from '@/state/workspace';
import { useWorkspaceList } from '@/state/workspaceList';
import { refreshWorkspaceInfo } from '@/state/workspaceInfo';
import { Button, Text } from '@/ui';

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
  const { colors } = useDesign();
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
    <UpgradeView
      body={body}
      checking={checking}
      stillRequired={stillRequired}
      others={others > 0}
      onGetUpdate={() => void WebBrowser.openBrowserAsync(identity.webUrl)}
      onCheckAgain={onCheckAgain}
      onOtherWorkspace={() => void beginAddWorkspace()}
      onLogOut={() => void endSession({ reason: 'user_logout' })}
    />
  );
  if (!overlay) return screen;
  return (
    <View
      style={[StyleSheet.absoluteFill, { backgroundColor: colors.background }]}
      accessibilityViewIsModal
      importantForAccessibility="yes">
      {screen}
    </View>
  );
}

/**
 * The update screen's content (audit 2.6): Agent Zefirca on the gradient, what is needed, ONE
 * filled "Get the update", "Check again" tonal, and the ways out as plain buttons (the wipe in
 * danger red, last).
 */
export function UpgradeView({
  body,
  checking,
  stillRequired,
  others,
  onGetUpdate,
  onCheckAgain,
  onOtherWorkspace,
  onLogOut,
}: {
  body: string;
  checking: boolean;
  stillRequired: boolean;
  others: boolean;
  onGetUpdate: () => void;
  onCheckAgain: () => void;
  onOtherWorkspace: () => void;
  onLogOut: () => void;
}) {
  return (
    <StatusScreen mascot="full" title={`Update ${identity.displayName}`} body={body}>
      <View style={styles.actions}>
        <Button title="Get the update" icon="openExternal" onPress={onGetUpdate} />
        <Button title="Check again" variant="tonal" loading={checking} onPress={onCheckAgain} />
        {stillRequired ? (
          <Text accessibilityRole="alert" variant="subheadline" tone="secondary" style={styles.center}>
            The workspace still needs a newer version.
          </Text>
        ) : null}
        {others ? <Button title="Use another workspace" variant="plain" onPress={onOtherWorkspace} /> : null}
        <Button title="Log out & wipe" variant="destructivePlain" onPress={onLogOut} />
      </View>
    </StatusScreen>
  );
}

const styles = StyleSheet.create({
  actions: { alignSelf: 'stretch', gap: 10, marginTop: 8 },
  center: { textAlign: 'center' },
});
