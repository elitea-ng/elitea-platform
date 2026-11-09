import * as WebBrowser from 'expo-web-browser';
import { Linking } from 'react-native';

import { effectiveSupportContact, supportContactLink, type SupportContact } from '@elitea/client-core/support';

import { useManagedConfig } from '@/security/managedConfig';
import { usePolicy } from '@/security/policy';
import { useWorkspaceInfo } from '@/state/workspaceInfo';

/**
 * The IT support line (ADR-0052 decision 7, W6.2) shown in About and Settings:
 * the device's managed `support_contact` (MDM), else the workspace's, else
 * none. Of the workspace's two copies the discovery document's wins once it
 * carries the field: it is re-read on every foreground, while the token
 * response's changes only at the next refresh. The token copy covers a launch
 * before discovery answers (it is persisted with the policy). Dormant: a workspace before client contract
 * 1.45 sends none, and without either the screens show no row.
 */
export function supportContactFrom(managed: string | null, policy: string | null, discovery: unknown): SupportContact | null {
  const text = effectiveSupportContact(managed, typeof discovery === 'string' ? discovery : policy);
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
