import { Link, Stack } from 'expo-router';
import { StyleSheet, Text, View } from 'react-native';

import { usePalette } from '@/constants/use-palette';
import { NotificationBell } from '@/features/notifications/NotificationBell';
import { ProjectChip } from '@/features/projects/ProjectChip';
import { useSelfIdentityRefresh } from '@/state/selfIdentity';

export default function AppLayout() {
  const palette = usePalette();
  // "Who am I", once per session: tells my messages from other people's.
  useSelfIdentityRefresh();
  return (
    <Stack>
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
      <Stack.Screen name="new" options={{ title: 'New conversation', presentation: 'modal' }} />
      <Stack.Screen name="notifications" options={{ title: 'Notifications' }} />
    </Stack>
  );
}

const styles = StyleSheet.create({
  headerActions: { flexDirection: 'row', gap: 16, alignItems: 'center' },
});
