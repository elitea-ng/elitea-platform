import * as AppIntents from 'expo-app-intents';
import { useEffect } from 'react';

import { log } from '@/lib/log';

/**
 * Siri still runs the app's intents while no workspace is signed in (the
 * intents only ask for an unlocked device), and the signed-in handler
 * (src/features/intents/useAppIntentHandler.ts) is not mounted then. Without
 * this, "Ask <app> <question>" said on the connect screen would wait in the
 * pending queue and be pre-typed into whichever workspace signs in next.
 * While signed out, every pending and every new invocation is cleared.
 */
function clear(): void {
  AppIntents.clearPendingInvocationsAsync().catch((error: unknown) => log.warn('pending intents not cleared', { error }));
}

export function useDiscardIntentsWhileSignedOut(signedOut: boolean): void {
  useEffect(() => {
    if (!signedOut || !AppIntents.isAvailable()) return;
    clear();
    const subscription = AppIntents.addAppIntentListener(clear);
    return () => subscription.remove();
  }, [signedOut]);
}
