import * as Linking from 'expo-linking';
import { useEffect } from 'react';
import { Alert } from 'react-native';

import { identity } from '@/config/identity';
import { log } from '@/lib/log';
import { useSession } from '@/state/session';
import { classifyIncomingLink, switchWorkspace } from '@/state/workspace';

/** URLs already acted on; `useLinkingURL` keeps returning the launch URL. */
const handled = new Set<string>();

/**
 * A connect link opened while a workspace is already connected must not
 * silently switch servers: switching wipes everything stored for the current
 * workspace. This asks first. While NOT connected, the /connect route reads the
 * link's `server` parameter itself, so nothing happens here.
 *
 * Mount it only once the session is hydrated (and unlocked).
 */
export function useIncomingConnectLink(enabled: boolean): void {
  const url = Linking.useLinkingURL();
  const serverOrigin = useSession((s) => s.serverOrigin);

  useEffect(() => {
    if (!enabled || !url || handled.has(url)) return;
    const action = classifyIncomingLink(url, serverOrigin, {
      connectSchemes: identity.connectSchemes,
      allowPrivateHttp: __DEV__,
    });
    if (action.kind !== 'switch') {
      if (action.kind !== 'connect') handled.add(url);
      return;
    }
    handled.add(url);
    Alert.alert(
      'Switch workspace?',
      `This signs you out of ${action.from} and deletes everything ${identity.displayName} stored for it on this device. Then you can check and confirm ${action.origin}.`,
      [
        { text: 'Cancel', style: 'cancel' },
        {
          text: 'Switch & wipe',
          style: 'destructive',
          onPress: () => {
            void switchWorkspace(action.origin).then((ok) => {
              if (!ok) log.warn('workspace switch did not complete');
            });
          },
        },
      ],
    );
  }, [enabled, url, serverOrigin]);
}
