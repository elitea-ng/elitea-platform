import { Stack } from 'expo-router';

import { useDesign } from '@/design/DesignProvider';
import { largeTitleOptions } from '@/ui';

export default function DesignChatsLayout() {
  const design = useDesign();
  return (
    <Stack>
      <Stack.Screen name="index" options={{ title: 'Chats', ...largeTitleOptions(design) }} />
    </Stack>
  );
}
