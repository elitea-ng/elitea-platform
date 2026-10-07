import { requireOptionalNativeModule } from 'expo-modules-core';
import type { Action } from 'expo-quick-actions';

/** The part of the (patched) native ExpoQuickActions module the package's JS does not expose. */
export interface QuickActionsNative {
  takeInitial?: () => Action | null;
}

export function quickActionsNative(): QuickActionsNative | null {
  return requireOptionalNativeModule<QuickActionsNative>('ExpoQuickActions');
}
