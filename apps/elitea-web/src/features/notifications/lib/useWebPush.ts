/**
 * The "Web push in this browser" switch's state (U3 contract §5): Main's
 * push config, this browser's support and permission, and the subscription.
 * `visible` is false when Main has push off (or no key), when the browser
 * cannot do push, and in the desktop app (it has native notices).
 */
import { useCallback, useEffect, useMemo, useState } from 'react';

import { useQuery } from '@tanstack/react-query';

import { getConfig } from '@/shared/config';
import { tauriInvoke } from '@/shared/desktop/hostBridge';

import { deletePushSubscription, getPushConfig, postPushSubscription } from '../api/webPush';
import { browserWebPushEnv, disableWebPush, enableWebPush, readWebPushState, webPushPlace, type WebPushEnv, type WebPushState } from './webPush';

export interface WebPushControl {
  readonly visible: boolean;
  readonly state: WebPushState | undefined;
  readonly busy: boolean;
  readonly failed: boolean;
  readonly setOn: (on: boolean) => void;
}

/** The app's base path and REST base, read the way `./routes.ts` reads them (the `app/` layer is out of reach). */
function appPaths(): { basename: string; serverUrl: string } {
  const result = getConfig();
  if (result.status !== 'ok') return { basename: '', serverUrl: '' };
  return { basename: import.meta.env.DEV ? '' : result.config.vite_base_uri, serverUrl: result.config.vite_server_url };
}

const IN_DESKTOP_BUILD = import.meta.env.MODE === 'desktop';

/** `env` stands in for the browser (tests). */
export function useWebPush(projectId: string, env?: WebPushEnv): WebPushControl {
  const pushEnv = useMemo(() => env ?? browserWebPushEnv(), [env]);
  const inDesktop = IN_DESKTOP_BUILD || tauriInvoke() !== undefined;
  const place = useMemo(() => {
    const { basename, serverUrl } = appPaths();
    return webPushPlace(basename, serverUrl, projectId);
  }, [projectId]);
  const config = useQuery({
    queryKey: ['notifications', 'push-config', projectId],
    queryFn: () => getPushConfig(projectId),
    enabled: !inDesktop && projectId !== '',
    staleTime: 5 * 60_000,
  });
  const [state, setState] = useState<WebPushState | undefined>(undefined);
  const [busy, setBusy] = useState(false);
  const [failed, setFailed] = useState(false);

  useEffect(() => {
    if (inDesktop) return;
    let live = true;
    void readWebPushState(pushEnv, place.scope)
      .catch((): WebPushState => 'off')
      .then((read) => {
        if (live) setState(read);
      });
    return () => {
      live = false;
    };
  }, [inDesktop, pushEnv, place.scope]);

  const publicKey = config.data?.public_key ?? '';
  const setOn = useCallback(
    (on: boolean) => {
      setBusy(true);
      setFailed(false);
      const step = on
        ? enableWebPush(pushEnv, place, publicKey, (body) => postPushSubscription(projectId, body))
        : disableWebPush(pushEnv, place.scope, (endpoint) => deletePushSubscription(projectId, endpoint));
      void step
        .then(setState, () => {
          setFailed(true);
          return readWebPushState(pushEnv, place.scope).then(setState, () => undefined);
        })
        .finally(() => setBusy(false));
    },
    [pushEnv, place, publicKey, projectId],
  );

  const visible = !inDesktop && config.data?.enabled === true && publicKey !== '' && state !== undefined && state !== 'unsupported';
  return { visible, state, busy, failed, setOn };
}
