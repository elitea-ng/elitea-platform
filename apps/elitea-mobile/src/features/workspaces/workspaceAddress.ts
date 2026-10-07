import * as Clipboard from 'expo-clipboard';

import { displayHost } from '@/features/connect/connectFlow';
import { haptic } from '@/ui';

/**
 * How a connected workspace's address reads in Settings (F6). An https workspace shows its host
 * ("ai.example.com"); anything else keeps its scheme ("http://10.0.0.5"), so a plain-http
 * development workspace never reads like a secure one (the same rule as connectFlow's
 * canonical-origin check).
 */
export function workspaceAddressTitle(origin: string): string {
  return /^https:\/\//i.test(origin) ? displayHost(origin) : origin;
}

/** The accessibility action that copies the full address (mirrors the row's long press). */
export const COPY_ADDRESS_ACTION = { name: 'copy', label: 'Copy address' } as const;

/** Copies the full origin (with its scheme) for support, with the copy haptic. */
export async function copyWorkspaceAddress(origin: string): Promise<void> {
  await Clipboard.setStringAsync(origin);
  haptic('copy');
}
