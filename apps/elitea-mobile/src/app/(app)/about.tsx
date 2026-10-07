import { router } from 'expo-router';
import * as WebBrowser from 'expo-web-browser';

import { getClientVersion } from '@/api/clientVersion';
import { identity } from '@/config/identity';
import { AboutView } from '@/features/settings/AboutView';

/** Settings → About Agent Zefir. */
export default function AboutScreen() {
  return (
    <AboutView
      version={getClientVersion()}
      onPrivacy={() => void WebBrowser.openBrowserAsync(identity.privacyUrl)}
      onSupport={() => void WebBrowser.openBrowserAsync(identity.supportUrl)}
      onDesignGallery={__DEV__ ? () => router.push('/design-gallery') : null}
    />
  );
}
