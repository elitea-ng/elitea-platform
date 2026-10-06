import * as Linking from 'expo-linking';
import { useEffect } from 'react';
import { Alert } from 'react-native';

import { identity } from '@/config/identity';
import { displayHost } from '@/features/connect/connectFlow';
import { log } from '@/lib/log';
import { useSession } from '@/state/session';
import { beginAddWorkspace, classifyIncomingLink, switchToWorkspace } from '@/state/workspace';
import { useWorkspaceList } from '@/state/workspaceList';

/** URLs already acted on; `useLinkingURL` keeps returning the launch URL. */
const handled = new Set<string>();

/**
 * A connect link opened while a workspace is active never switches silently,
 * and never wipes anything: a workspace already connected on this device is
 * switched to, a new one is added next to the others (the connect screen
 * then checks it and asks the user to confirm its host). Both ask first,
 * because the switch stops whatever is running in the current workspace.
 * While no workspace is active, the /connect route reads the link's `server`
 * parameter itself, so nothing happens here.
 *
 * Mount it only once the session is hydrated (and unlocked).
 */
export function useIncomingConnectLink(enabled: boolean): void {
  const url = Linking.useLinkingURL();
  const serverOrigin = useSession((s) => s.serverOrigin);
  const known = useWorkspaceList();

  useEffect(() => {
    if (!enabled || !url || handled.has(url)) return;
    const action = classifyIncomingLink(
      url,
      serverOrigin,
      { connectSchemes: identity.connectSchemes, allowPrivateHttp: __DEV__ },
      known,
    );
    if (action.kind !== 'switch' && action.kind !== 'add') {
      if (action.kind !== 'connect') handled.add(url);
      return;
    }
    handled.add(url);
    const from = displayHost(action.from);
    const to = displayHost(action.origin);
    if (action.kind === 'switch') {
      Alert.alert('Switch workspace?', `Switch from ${from} to ${to}? Nothing is deleted; you can switch back any time.`, [
        { text: 'Cancel', style: 'cancel' },
        {
          text: `Open ${to}`,
          onPress: () => {
            void switchToWorkspace(action.workspaceId).then((ok) => {
              if (!ok) log.warn('workspace switch did not complete');
            });
          },
        },
      ]);
      return;
    }
    Alert.alert(
      'Add workspace?',
      `Connect ${identity.displayName} to ${to} as well? ${from} stays connected and keeps its data; you will check and confirm the new workspace first.`,
      [
        { text: 'Cancel', style: 'cancel' },
        { text: 'Add workspace', onPress: () => void beginAddWorkspace(action.origin) },
      ],
    );
  }, [enabled, url, serverOrigin, known]);
}
