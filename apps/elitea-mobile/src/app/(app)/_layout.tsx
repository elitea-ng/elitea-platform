import { Link, Stack } from 'expo-router';
import { Text } from 'react-native';

import { usePalette } from '@/constants/use-palette';

export default function AppLayout() {
  const palette = usePalette();
  return (
    <Stack>
      <Stack.Screen
        name="index"
        options={{
          title: 'Conversations',
          headerRight: () => (
            <Link href="/settings" accessibilityLabel="Settings">
              <Text style={{ color: palette.accent, fontSize: 16 }}>Settings</Text>
            </Link>
          ),
        }}
      />
      <Stack.Screen name="settings" options={{ title: 'Settings' }} />
    </Stack>
  );
}
