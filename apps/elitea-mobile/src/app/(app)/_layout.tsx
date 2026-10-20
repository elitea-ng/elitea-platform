import { router, Stack } from 'expo-router';

import { useDesign } from '@/design/DesignProvider';
import { Button, sheetOptions } from '@/ui';

import { useQuickActions } from '@/features/quickActions/useQuickActions';
import { useWidgetSnapshot } from '@/features/surfaces/useWidgetSnapshot';
import { useAppIntentHandler } from '@/features/intents/useAppIntentHandler';
import { workspaceHeaderOptions } from '@/features/workspaces/workspaceHeader';
import { useSelfIdentityRefresh } from '@/state/selfIdentity';

// A screen opened straight from outside the app (a widget, a Live Activity, a link) keeps the
// tabs (the conversation list) beneath it, so Back works.
export const unstable_settings = { anchor: '(tabs)' };

export default function AppLayout() {
  // "Who am I", once per session: tells my messages from other people's.
  useSelfIdentityRefresh();
  // Home-screen quick actions for the active workspace (titles only when the policy allows).
  useQuickActions();
  // Home-screen and lock-screen widgets: counts of the active workspace (titles only when the policy allows).
  useWidgetSnapshot();
  // Siri and Shortcuts: open-app intents only (agent names only when the policy allows titles).
  useAppIntentHandler();
  const design = useDesign();
  // The switchers are native sheets with detents (§4.10): grabber, swipe-down and Android back
  // close them, and Done is there for VoiceOver and Switch Control users (audit 2.9).
  const switcher = {
    ...sheetOptions(design, 'half'),
    headerRight: () => <Button title="Done" variant="plain" size="small" onPress={() => router.back()} />,
  };
  return (
    // Every signed-in screen names the active workspace's host in its header. The tabs carry
    // their own stacks (and headers); a thread, an agent page and the sheets sit above them.
    <Stack screenOptions={workspaceHeaderOptions}>
      {/* Its title is the back label of every screen above the tabs. */}
      <Stack.Screen name="(tabs)" options={{ headerShown: false, title: 'Back' }} />
      <Stack.Screen name="c/[id]" options={{ title: 'Conversation' }} />
      <Stack.Screen name="projects" options={{ title: 'Projects', ...switcher }} />
      <Stack.Screen name="workspaces" options={{ title: 'Workspaces', ...switcher }} />
      {/* Rename a chat (client contract 1.4): a sheet with Cancel and Save in its bar (§4.10). */}
      <Stack.Screen name="rename" options={{ title: 'Rename', ...sheetOptions(design, 'half') }} />
      {/* A modal always has a visible way out (audit 2.10): Cancel for VoiceOver and Switch Control. */}
      <Stack.Screen
        name="new"
        options={{
          title: 'New chat',
          presentation: 'modal',
          headerLeft: () => <Button title="Cancel" variant="plain" size="small" onPress={() => router.back()} />,
        }}
      />
      <Stack.Screen name="notifications" options={{ title: 'Notifications' }} />
      <Stack.Screen name="memory" options={{ title: 'Memory' }} />
      <Stack.Screen name="devices" options={{ title: 'Signed-in devices' }} />
      <Stack.Screen name="about" options={{ title: 'About' }} />
      {/* An agent from Discover opens as a sheet over the catalogue (docs/screens.md §6), at full
          height: its one action, Start chat, follows a description of any length (S5). */}
      <Stack.Screen name="discover/[id]" options={{ title: 'Agent', ...switcher, ...sheetOptions(design, 'detail') }} />
    </Stack>
  );
}
