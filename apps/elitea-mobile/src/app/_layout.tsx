import { DarkTheme, DefaultTheme, Stack, ThemeProvider } from 'expo-router';
import { StatusBar } from 'expo-status-bar';
import { useColorScheme } from 'react-native';

import { WorkspaceProvider, useWorkspace } from '@/state/workspace';

export default function RootLayout() {
  const colorScheme = useColorScheme();
  return (
    <WorkspaceProvider>
      <ThemeProvider value={colorScheme === 'dark' ? DarkTheme : DefaultTheme}>
        <RootStack />
        <StatusBar style="auto" />
      </ThemeProvider>
    </WorkspaceProvider>
  );
}

/**
 * Without a configured server every route resolves to /connect; with one, the
 * signed-in (app) group is reachable. Stack.Protected redirects automatically.
 *
 * TODO: biometric lock gate and app-switcher privacy cover (docs/design.md).
 * TODO: a connect link received while already connected must ask before
 * switching servers (a switch wipes local data).
 */
function RootStack() {
  const { serverOrigin } = useWorkspace();
  const connected = serverOrigin !== null;
  return (
    <Stack screenOptions={{ headerShown: false }}>
      <Stack.Protected guard={connected}>
        <Stack.Screen name="(app)" />
      </Stack.Protected>
      <Stack.Protected guard={!connected}>
        <Stack.Screen name="connect" />
      </Stack.Protected>
    </Stack>
  );
}
