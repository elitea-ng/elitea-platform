import { QueryClientProvider } from '@tanstack/react-query';
import { DarkTheme, DefaultTheme, Stack, ThemeProvider } from 'expo-router';
import * as SplashScreen from 'expo-splash-screen';
import { StatusBar } from 'expo-status-bar';
import { useEffect, useMemo, useState, type ReactNode } from 'react';
import { KeyboardProvider } from 'react-native-keyboard-controller';

import { StatusScreen } from '@/components/StatusScreen';
import { DesignProvider, useDesign } from '@/design/DesignProvider';
import { sheetOptions } from '@/ui';
import { UpgradeRequired } from '@/components/UpgradeRequired';
import { queryClient, wireFocusManager, wireUpgradeRecovery } from '@/api/queryClient';
import { startOfflineStore } from '@/db/startOfflineStore';
import { wireNetwork } from '@/lib/network';
import { startOutbox } from '@/sync/appOutbox';
import { startSync } from '@/sync/appSync';
import { LockGate } from '@/security/lock/LockGate';
import { startManagedConfig } from '@/security/managedConfig';
import { PrivacyCover } from '@/security/privacy/PrivacyCover';
import { usePrivacyProtection } from '@/security/privacy/usePrivacyProtection';
import { useLockController } from '@/security/lock/useLockController';
import { useIncomingConnectLink } from '@/linking/useIncomingConnectLink';
import { hydrateSession } from '@/state/hydrate';
import { useSession } from '@/state/session';
import { refreshWorkspaceInfo } from '@/state/workspaceInfo';
import { startForegroundRefresh } from '@/state/foregroundRefresh';
import { startWorkspaceCheckIns } from '@/state/workspaceCheckIn';
import { createSplashGate } from '@/state/splashGate';
import { sweepUploadTemp } from '@/api/attachments/upload';
import { sweepDownloadTemp } from '@/features/chat/attachments/opener';
import { sweepPickerCopies } from '@/features/chat/attachments/pickers';
// Registers the wipe hooks of the system surfaces (widget snapshot) before any session can end.
import '@/features/surfaces/surfaceWriter';
import '@/features/chat/attachments/tempWipe';
import '@/features/intents/agentCatalog';
import { useDiscardIntentsWhileSignedOut } from '@/features/intents/discardIntentsWhileSignedOut';
import { startTurnActivities } from '@/features/surfaces/turnActivities';

// MDM AppConfig first: its lock floor must be in place before the launch lock decision.
startManagedConfig();
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
// Workspaces put aside are checked in with their servers (revocation, policy).
startWorkspaceCheckIns();
// Live Activities of running answers (local only); ends any a previous process left behind.
startTurnActivities();

// The native splash stays up until the session is restored, so the first
// screen is the right one (no flash of /connect).
void SplashScreen.preventAutoHideAsync().catch(() => {});
const updateSplash = createSplashGate(() => SplashScreen.hideAsync());

export default function RootLayout() {
  usePrivacyProtection();
  return (
    <QueryClientProvider client={queryClient}>
      {/* One design for the whole app: scheme, Zefir tokens, the corrected workspace accent,
          accessibility modifiers (docs/design-system.md §6.1). */}
      <DesignProvider>
        <KeyboardProvider>
          <NavigationTheme>
            <RootStack />
            <PrivacyCover />
          </NavigationTheme>
        </KeyboardProvider>
      </DesignProvider>
    </QueryClientProvider>
  );
}

/**
 * Navigation chrome (headers, cards, the tab bar's tint) from the same tokens as the screens:
 * the Zefir neutrals of the scheme and the corrected workspace accent. The status bar follows
 * the scheme (dark content on the light grounds, light on the night ones).
 */
function NavigationTheme({ children }: { children: ReactNode }) {
  const { scheme, colors } = useDesign();
  const navigationTheme = useMemo(() => {
    const base = scheme === 'dark' ? DarkTheme : DefaultTheme;
    return {
      ...base,
      colors: {
        ...base.colors,
        primary: colors.accent,
        background: colors.background,
        card: colors.background,
        text: colors.textPrimary,
        border: colors.separator,
        notification: colors.badge,
      },
    };
  }, [scheme, colors]);
  return (
    <ThemeProvider value={navigationTheme}>
      {children}
      <StatusBar style={scheme === 'dark' ? 'light' : 'dark'} />
    </ThemeProvider>
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
 * Several workspaces can be connected; one is active. A switch (nothing is
 * wiped) shows "Switching workspace…" and mounts the new workspace's stack
 * from scratch. A connect link received while connected asks before
 * switching to (or adding) that workspace (`useIncomingConnectLink`).
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
  const switching = useSession((s) => s.switching);
  const serverOrigin = useSession((s) => s.serverOrigin);
  const workspaceId = useSession((s) => s.workspaceId);
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
  const lock = useLockController(signedIn, hydrated, workspaceId);
  const lockDecided = !signedIn || lock.status !== 'deciding';

  // The signed-in stack mounts after the first unlock and then stays mounted,
  // per workspace: a switch starts the new workspace's stack from scratch.
  const [stackReady, setStackReady] = useState<string | null>(null);
  const stackKey = signedIn ? (workspaceId ?? 'workspace') : null;
  if (stackKey !== null && lock.status === 'unlocked' && stackReady !== stackKey) setStackReady(stackKey);
  if (stackReady !== null && stackReady !== stackKey && (stackKey === null || lock.status !== 'unlocked')) {
    setStackReady(null);
  }

  useEffect(() => {
    updateSplash(hydrated && lockDecided);
  }, [hydrated, lockDecided]);
  useIncomingConnectLink(hydrated && !ending && !switching && (!signedIn || lock.status === 'unlocked'));
  // Siri runs while signed out too: nothing queued then reaches the next workspace.
  useDiscardIntentsWhileSignedOut(hydrated && !signedIn);

  if (!hydrated) return <StatusScreen busy />;
  if (ending) return <StatusScreen busy title="Signing out…" />;
  if (switching) return <StatusScreen busy title="Switching workspace…" />;

  // The update screen covers the app instead of replacing it (DEF-R4): the
  // navigation stack and the lock stay mounted and untouched, so a 426 never
  // restarts the lock, and the app is back where it was once it clears.
  const upgrade = upgradeRequired ? <UpgradeRequired minVersion={upgradeRequired.minVersion} overlay /> : null;
  if (signedIn && stackReady !== stackKey) {
    return (
      <>
        <LockGate state={lock} />
        {upgrade}
      </>
    );
  }

  return (
    <>
      <RoutedStack key={workspaceId ?? 'none'} connected={connected} signedIn={signedIn} />
      {signedIn ? <LockGate state={lock} /> : null}
      {upgrade}
    </>
  );
}

function RoutedStack({ connected, signedIn }: { connected: boolean; signedIn: boolean }) {
  const design = useDesign();
  return (
    <Stack screenOptions={{ headerShown: false }}>
      {/* Development builds only: every kit component in light/dark/branded (screenshots). */}
      <Stack.Protected guard={__DEV__}>
        <Stack.Screen name="design-gallery" options={{ headerShown: true, title: 'Design gallery' }} />
        <Stack.Screen name="design-tabs" options={{ headerShown: false }} />
        <Stack.Screen name="design-composer" options={{ headerShown: true, title: 'Conversation' }} />
        <Stack.Screen name="design-chat" options={{ headerShown: true, title: 'Conversation' }} />
        <Stack.Screen name="design-sheet" options={{ headerShown: true, title: 'Workspace', ...sheetOptions(design) }} />
        <Stack.Screen name="design-entry" options={{ headerShown: false }} />
        <Stack.Screen name="design-workspaces" options={{ headerShown: true, title: 'Workspaces', ...sheetOptions(design) }} />
        <Stack.Screen name="design-projects" options={{ headerShown: true, title: 'Projects', ...sheetOptions(design) }} />
      </Stack.Protected>
      <Stack.Protected guard={signedIn}>
        <Stack.Screen name="(app)" />
      </Stack.Protected>
      <Stack.Protected guard={connected && !signedIn}>
        <Stack.Screen name="sign-in" options={{ title: 'Sign in' }} />
      </Stack.Protected>
      <Stack.Protected guard={!connected}>
        <Stack.Screen name="connect" options={{ title: 'Connect' }} />
      </Stack.Protected>
    </Stack>
  );
}
