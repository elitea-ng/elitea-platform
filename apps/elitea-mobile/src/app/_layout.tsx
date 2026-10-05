import { QueryClientProvider } from '@tanstack/react-query';
import { DarkTheme, DefaultTheme, Stack, ThemeProvider } from 'expo-router';
import * as SplashScreen from 'expo-splash-screen';
import { StatusBar } from 'expo-status-bar';
import { useEffect, useMemo, useState } from 'react';
import { useColorScheme } from 'react-native';

import { StatusScreen } from '@/components/StatusScreen';
import { usePalette } from '@/constants/use-palette';
import { UpgradeRequired } from '@/components/UpgradeRequired';
import { queryClient, wireFocusManager, wireUpgradeRecovery } from '@/api/queryClient';
import { startOfflineStore } from '@/db/startOfflineStore';
import { wireNetwork } from '@/lib/network';
import { startOutbox } from '@/sync/appOutbox';
import { startSync } from '@/sync/appSync';
import { LockGate } from '@/security/lock/LockGate';
import { PrivacyCover } from '@/security/privacy/PrivacyCover';
import { usePrivacyProtection } from '@/security/privacy/usePrivacyProtection';
import { useLockController } from '@/security/lock/useLockController';
import { useIncomingConnectLink } from '@/linking/useIncomingConnectLink';
import { hydrateSession } from '@/state/hydrate';
import { useSession } from '@/state/session';
import { refreshWorkspaceInfo } from '@/state/workspaceInfo';
import { startForegroundRefresh } from '@/state/foregroundRefresh';
import { createSplashGate } from '@/state/splashGate';
import { sweepUploadTemp } from '@/api/attachments/upload';
import { sweepDownloadTemp } from '@/features/chat/attachments/opener';
import { sweepPickerCopies } from '@/features/chat/attachments/pickers';

wireFocusManager();
wireUpgradeRecovery();
// The encrypted offline store follows the session, the policy and the device lock.
startOfflineStore();
startSync();
// Connectivity: react-query's online state, the outbox drain and sync on reconnect.
wireNetwork();
startOutbox();
// Attachment copies a crash or a kill left behind (downloads opened, chunks staged, files picked).
sweepDownloadTemp();
sweepUploadTemp();
sweepPickerCopies();
// The public policy and minimum version are re-read on every foreground.
startForegroundRefresh();

// The native splash stays up until the session is restored, so the first
// screen is the right one (no flash of /connect).
void SplashScreen.preventAutoHideAsync().catch(() => {});
const updateSplash = createSplashGate(() => SplashScreen.hideAsync());

export default function RootLayout() {
  const colorScheme = useColorScheme();
  const palette = usePalette();
  usePrivacyProtection();
  // Navigation chrome (headers, cards) follows the workspace brand too.
  const navigationTheme = useMemo(() => {
    const base = colorScheme === 'dark' ? DarkTheme : DefaultTheme;
    return {
      ...base,
      colors: {
        ...base.colors,
        primary: palette.accent,
        background: palette.background,
        card: palette.background,
        text: palette.text,
        border: palette.border,
        notification: palette.danger,
      },
    };
  }, [colorScheme, palette]);
  return (
    <QueryClientProvider client={queryClient}>
      <ThemeProvider value={navigationTheme}>
        <RootStack />
        <PrivacyCover />
        <StatusBar style="auto" />
      </ThemeProvider>
    </QueryClientProvider>
  );
}

/**
 * Routing guards. Nothing routes until launch-time storage work is done
 * (`hydrated`); while `endSession` runs, a neutral "Signing out…" screen
 * replaces everything; after a 426 the blocking "Update" screen covers
 * everything (the stack and the lock stay mounted underneath).
 * Then: no server → /connect; server but no credential → /sign-in;
 * signed in → the (app) group. Stack.Protected redirects automatically.
 *
 * A connect link received while connected asks before switching servers
 * (`useIncomingConnectLink`): a switch wipes local data.
 *
 * Device lock: a signed-in session is guarded by the lock controller. The
 * signed-in stack mounts only after the first unlock; after that a relock
 * covers it (LockGate) so navigation state survives.
 *
 * Screen privacy (root layout): policy-driven screenshot blocking and the
 * app-switcher cover, above everything including the lock screen.
 */
function RootStack() {
  const hydrated = useSession((s) => s.hydrated);
  const ending = useSession((s) => s.ending);
  const serverOrigin = useSession((s) => s.serverOrigin);
  const upgradeRequired = useSession((s) => s.upgradeRequired);
  const credentialKind = useSession((s) => s.credential?.kind ?? null);

  useEffect(() => {
    void hydrateSession();
  }, []);

  // Re-read the workspace's public discovery document once per connected
  // origin (launch, connect, switch): it carries the native sign-in
  // endpoints, the public policy and the minimum app version. Never blocks.
  useEffect(() => {
    if (hydrated && serverOrigin !== null) void refreshWorkspaceInfo();
  }, [hydrated, serverOrigin]);

  const connected = serverOrigin !== null;
  const signedIn = connected && credentialKind !== null;
  const lock = useLockController(signedIn, hydrated);
  const lockDecided = !signedIn || lock.status !== 'deciding';

  // The signed-in stack mounts after the first unlock and then stays mounted.
  const [stackReady, setStackReady] = useState(false);
  if (signedIn && lock.status === 'unlocked' && !stackReady) setStackReady(true);
  if (!signedIn && stackReady) setStackReady(false);

  useEffect(() => {
    updateSplash(hydrated && lockDecided);
  }, [hydrated, lockDecided]);
  useIncomingConnectLink(hydrated && !ending && (!signedIn || lock.status === 'unlocked'));

  if (!hydrated) return <StatusScreen busy />;
  if (ending) return <StatusScreen busy title="Signing out…" />;

  // The update screen covers the app instead of replacing it (DEF-R4): the
  // navigation stack and the lock stay mounted and untouched, so a 426 never
  // restarts the lock, and the app is back where it was once it clears.
  const upgrade = upgradeRequired ? <UpgradeRequired minVersion={upgradeRequired.minVersion} overlay /> : null;
  if (signedIn && !stackReady) {
    return (
      <>
        <LockGate state={lock} />
        {upgrade}
      </>
    );
  }

  return (
    <>
      <RoutedStack connected={connected} signedIn={signedIn} />
      {signedIn ? <LockGate state={lock} /> : null}
      {upgrade}
    </>
  );
}

function RoutedStack({ connected, signedIn }: { connected: boolean; signedIn: boolean }) {
  return (
    <Stack screenOptions={{ headerShown: false }}>
      <Stack.Protected guard={signedIn}>
        <Stack.Screen name="(app)" />
      </Stack.Protected>
      <Stack.Protected guard={connected && !signedIn}>
        <Stack.Screen name="sign-in" />
      </Stack.Protected>
      <Stack.Protected guard={!connected}>
        <Stack.Screen name="connect" />
      </Stack.Protected>
    </Stack>
  );
}
