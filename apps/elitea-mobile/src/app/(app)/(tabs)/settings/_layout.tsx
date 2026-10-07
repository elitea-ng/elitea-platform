import { Stack } from 'expo-router';

import { workspaceHeaderOptions } from '@/features/workspaces/workspaceHeader';

export default function SettingsTabLayout() {
  return (
    <Stack screenOptions={workspaceHeaderOptions}>
      <Stack.Screen name="index" options={{ title: 'Settings' }} />
    </Stack>
  );
}
