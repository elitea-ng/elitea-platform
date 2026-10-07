import { Redirect, router, Stack } from 'expo-router';
import { ScrollView, StyleSheet } from 'react-native';

import { useDesign } from '@/design/DesignProvider';
import { Button, ListRow, ListSection, Text } from '@/ui';

/**
 * Development builds only: a native sheet as the kit presents one (`sheetOptions`, set on the
 * route in the root stack): form sheet with detents and a grabber on iOS, an M3 modal bottom
 * sheet on Android, a native header with a text Done.
 */
export default function DesignSheetRoute() {
  const { colors } = useDesign();
  if (!__DEV__) return <Redirect href="/" />;
  return (
    <ScrollView style={{ backgroundColor: colors.elevated }} contentContainerStyle={styles.content}>
      <Stack.Screen options={{ title: 'Workspace', headerShown: true }} />
      <Text variant="callout" tone="secondary">
        Swipe down, drag the grabber, or use Android back to close.
      </Text>
      <ListSection title="Projects">
        <ListRow title="Personal" selected onPress={() => router.back()} />
        <ListRow title="Marketing" selected={false} onPress={() => router.back()} />
      </ListSection>
      <Button title="Switch workspace…" variant="tonal" onPress={() => router.back()} />
    </ScrollView>
  );
}

const styles = StyleSheet.create({ content: { padding: 16, gap: 20 } });
