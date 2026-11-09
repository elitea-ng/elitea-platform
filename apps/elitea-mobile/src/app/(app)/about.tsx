import { router } from 'expo-router';
import * as WebBrowser from 'expo-web-browser';

import { getClientVersion } from '@/api/clientVersion';
import { identity } from '@/config/identity';
import { openSupportContact, useSupportContact } from '@/security/supportContact';
import { AboutView } from '@/features/settings/AboutView';

/** Settings → About Elitea AI. */
export default function AboutScreen() {
  const supportContact = useSupportContact();
  return (
    <AboutView
      version={getClientVersion()}
      onPrivacy={() => void WebBrowser.openBrowserAsync(identity.privacyUrl)}
      onSupport={() => void WebBrowser.openBrowserAsync(identity.supportUrl)}
      supportContact={supportContact}
      onSupportContact={supportContact ? () => openSupportContact(supportContact) : null}
      onDesignGallery={__DEV__ ? () => router.push('/design-gallery') : null}
    />
  );
}
