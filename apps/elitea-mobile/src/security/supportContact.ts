import * as WebBrowser from 'expo-web-browser';
import { Linking } from 'react-native';

import { effectiveSupportContact, supportContactLink, type SupportContact } from '@elitea/client-core/support';

import { useManagedConfig } from '@/security/managedConfig';
import { usePolicy } from '@/security/policy';
import { useWorkspaceInfo } from '@/state/workspaceInfo';

/**
 * The IT support line (ADR-0052 decision 7, W6.2) shown in About and Settings:
 * the device's managed `support_contact` (MDM), else the workspace's
 * (`client_policy.support_contact` of the token response, else of the
 * discovery document), else none. Dormant: a workspace before client contract
 * 1.45 sends none, and without either the screens show no row.
 */
export function supportContactFrom(managed: string | null, policy: string | null, discovery: unknown): SupportContact | null {
  const text = effectiveSupportContact(managed, policy ?? discovery);
  return text === null ? null : supportContactLink(text);
}

export function useSupportContact(): SupportContact | null {
  const managed = useManagedConfig().supportContact;
  const policy = usePolicy().policy.support_contact;
  const discovery = useWorkspaceInfo()?.discovery.client_policy.support_contact;
  return supportContactFrom(managed, policy, discovery);
}

/** Opens the line's link: an https address in the in-app browser, `mailto:` and `tel:` with the system. */
export function openSupportContact(contact: SupportContact): void {
  const href = contact.href;
  if (href === null) return;
  if (href.startsWith('https:')) void WebBrowser.openBrowserAsync(href);
  else void Linking.openURL(href).catch(() => undefined);
}
