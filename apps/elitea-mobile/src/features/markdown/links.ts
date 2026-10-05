import * as WebBrowser from 'expo-web-browser';
import { Alert } from 'react-native';

import { originOf } from '@/api/paths';

/**
 * Links in agent output are untrusted (prompt injection can plant them).
 * Only absolute `https:` links open (`http:` too in development builds),
 * always in the system browser, never with the workspace token, and only
 * after the user has seen the host.
 */

const CONTROL = /[\u0000-\u001f\u007f\s]/;

export function safeLinkHost(href: string, allowHttp: boolean = __DEV__): string | null {
  if (typeof href !== 'string' || href.length > 2048 || CONTROL.test(href)) return null;
  const scheme = /^([a-z][a-z0-9+.-]*):/i.exec(href)?.[1]?.toLowerCase();
  if (scheme !== 'https' && !(allowHttp && scheme === 'http')) return null;
  const origin = originOf(href);
  if (origin === null) return null;
  return origin.replace(/^https?:\/\//, '');
}

export type LinkOpener = (href: string, host: string) => void;

/** Asks before leaving the app, then opens the system browser. */
export const confirmAndOpenLink: LinkOpener = (href, host) => {
  Alert.alert('Open link?', host, [
    { text: 'Cancel', style: 'cancel' },
    {
      text: 'Open',
      onPress: () => {
        void WebBrowser.openBrowserAsync(href).catch(() => {});
      },
    },
  ]);
};
