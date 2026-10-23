import { ScrollView, StyleSheet, View } from 'react-native';

import { EliteaMark } from '@/components/EliteaMark';
import { useDesign } from '@/design/DesignProvider';
import { ListRow, ListSection, Text, useLayoutMargin } from '@/ui';

export interface AboutViewProps {
  version: string;
  onPrivacy: () => void;
  onSupport: () => void;
  /** Development builds: the design gallery. */
  onDesignGallery?: (() => void) | null;
}

/**
 * Settings → About Elitea AI: the Elitea mark (design-system §5), the app's name and version, and the
 * policy and support links (opened in the in-app browser). The app is always "Elitea AI" here,
 * never the workspace's name.
 */
export function AboutView({ version, onPrivacy, onSupport, onDesignGallery }: AboutViewProps) {
  const { colors, accessibilityLayout } = useDesign();
  const margin = useLayoutMargin();
  return (
    <ScrollView
      contentInsetAdjustmentBehavior="automatic"
      style={{ backgroundColor: colors.background }}
      contentContainerStyle={[styles.content, { paddingHorizontal: margin }]}>
      <View style={styles.hero}>
        <EliteaMark size={accessibilityLayout ? 72 : 112} decorative />
        <Text variant="title1" heading style={styles.center}>
          Elitea AI
        </Text>
        <Text variant="footnote" tone="secondary" style={styles.center}>
          Version {version}
        </Text>
        <Text variant="callout" tone="secondary" style={styles.center}>
          Chat with the agents of your workspace, wherever you are.
        </Text>
      </View>
      <ListSection>
        <ListRow icon="info" title="Privacy policy" onPress={onPrivacy} />
        <ListRow icon="link" title="Support" onPress={onSupport} />
        {onDesignGallery ? <ListRow icon="palette" title="Design gallery" subtitle="Development builds only" onPress={onDesignGallery} /> : null}
      </ListSection>
    </ScrollView>
  );
}

const styles = StyleSheet.create({
  content: { paddingVertical: 24, paddingBottom: 48, gap: 28 },
  hero: { alignItems: 'center', gap: 8 },
  center: { textAlign: 'center' },
});
