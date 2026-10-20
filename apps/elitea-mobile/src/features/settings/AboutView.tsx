import { ScrollView, StyleSheet, View } from 'react-native';

import { Mascot } from '@/components/Mascot';
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
 * Settings → About Agent Zefir: the one place the mascot is named (design-system §5: the full
 * figure at 160, "Agent Zefirca, the Agent Zefir mascot"), the app's name and version, and the
 * policy and support links (opened in the in-app browser). The app is always "Agent Zefir" here,
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
        <Mascot variant={accessibilityLayout ? 'bust' : 'full'} size={accessibilityLayout ? 96 : 160} label="Agent Zefirca, the Agent Zefir mascot" />
        <Text variant="title1" heading style={styles.center}>
          Agent Zefir
        </Text>
        <Text variant="footnote" tone="secondary" style={styles.center}>
          Version {version}
        </Text>
        <Text variant="callout" tone="secondary" style={styles.center}>
          Chat with the agents of your workspace, wherever you are.
        </Text>
      </View>
      <ListSection footer="Agent Zefirca is the Agent Zefir mascot.">
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
