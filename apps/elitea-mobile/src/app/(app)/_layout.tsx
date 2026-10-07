import { Link, Stack } from 'expo-router';
import { StyleSheet, Text, View } from 'react-native';

import { usePalette } from '@/constants/use-palette';
import { NotificationBell } from '@/features/notifications/NotificationBell';
import { ProjectChip } from '@/features/projects/ProjectChip';
import { useQuickActions } from '@/features/quickActions/useQuickActions';
import { useWidgetSnapshot } from '@/features/surfaces/useWidgetSnapshot';
import { useAppIntentHandler } from '@/features/intents/useAppIntentHandler';
import { WorkspaceTitle } from '@/features/workspaces/WorkspaceTitle';
import { useSelfIdentityRefresh } from '@/state/selfIdentity';

// A screen opened straight from outside the app (a widget, a Live Activity, a link) keeps the
// conversation list beneath it, so Back works.
export const unstable_settings = { initialRouteName: 'index' };

export default function AppLayout() {
  const palette = usePalette();
  // "Who am I", once per session: tells my messages from other people's.
  useSelfIdentityRefresh();
  // Home-screen quick actions for the active workspace (titles only when the policy allows).
  useQuickActions();
  // Home-screen and lock-screen widgets: counts of the active workspace (titles only when the policy allows).
  useWidgetSnapshot();
  // Siri and Shortcuts: open-app intents only (agent names only when the policy allows titles).
  useAppIntentHandler();
  return (
    // Every signed-in screen names the active workspace's host in its header.
    <Stack screenOptions={{ headerTitle: (props) => <WorkspaceTitle title={props.children} /> }}>
      <Stack.Screen
        name="index"
        options={{
          title: 'Conversations',
          headerLeft: () => <ProjectChip />,
          headerRight: () => (
            <View style={styles.headerActions}>
              <NotificationBell />
              <Link href="/new" accessibilityLabel="New conversation">
                <Text style={{ color: palette.accent, fontSize: 16, fontWeight: '600' }}>New</Text>
              </Link>
              <Link href="/settings" accessibilityLabel="Settings">
                <Text style={{ color: palette.accent, fontSize: 16 }}>Settings</Text>
              </Link>
            </View>
          ),
        }}
      />
      <Stack.Screen name="c/[id]" options={{ title: 'Conversation' }} />
      <Stack.Screen name="settings" options={{ title: 'Settings' }} />
      <Stack.Screen name="projects" options={{ title: 'Projects', presentation: 'modal' }} />
      <Stack.Screen name="workspaces" options={{ title: 'Workspaces', presentation: 'modal' }} />
      <Stack.Screen name="new" options={{ title: 'New conversation', presentation: 'modal' }} />
      <Stack.Screen name="notifications" options={{ title: 'Notifications' }} />
      <Stack.Screen name="memory" options={{ title: 'Memory' }} />
    </Stack>
  );
}

const styles = StyleSheet.create({
  headerActions: { flexDirection: 'row', gap: 16, alignItems: 'center' },
});
