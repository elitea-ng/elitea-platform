import { Link, Stack } from 'expo-router';
import { StyleSheet, Text, View } from 'react-native';

import { usePalette } from '@/constants/use-palette';
import { NotificationBell } from '@/features/notifications/NotificationBell';
import { ProjectChip } from '@/features/projects/ProjectChip';
import { workspaceHeaderOptions } from '@/features/workspaces/workspaceHeader';

export default function ChatsTabLayout() {
  const palette = usePalette();
  return (
    <Stack screenOptions={workspaceHeaderOptions}>
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
            </View>
          ),
        }}
      />
    </Stack>
  );
}

const styles = StyleSheet.create({
  headerActions: { flexDirection: 'row', gap: 16, alignItems: 'center' },
});
