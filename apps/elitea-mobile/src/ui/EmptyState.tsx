import { StyleSheet, View, type ViewProps } from 'react-native';

import { Mascot } from '@/components/Mascot';
import { useDesign } from '@/design/DesignProvider';

import { Button } from './Button';
import { Text } from './Text';

export interface EmptyStateProps {
  title: string;
  body?: string;
  /** One primary action (a filled pill). */
  action?: { title: string; onPress: () => void };
  /**
   * The mascot (design-system §4.9, §5): `full` (160) in roomy containers, `bust` (96) in short
   * ones, `none` for errors that involve data loss, security or approval states.
   */
  mascot?: 'full' | 'bust' | 'none';
  style?: ViewProps['style'];
}

/**
 * The empty state (§4.9): a centred column with the mascot (decorative: the title carries the
 * meaning), a title2 heading, one callout line and one primary action. At accessibility text
 * sizes the mascot drops to the bust so the text keeps the room.
 */
export function EmptyState({ title, body, action, mascot = 'full', style }: EmptyStateProps) {
  const design = useDesign();
  const variant = mascot === 'none' ? null : design.accessibilityLayout ? 'bust' : mascot;
  return (
    <View style={[styles.wrap, { padding: design.space.screenGutter }, style]}>
      {variant ? <Mascot variant={variant} size={variant === 'full' ? 160 : 96} decorative /> : null}
      <Text variant="title2" heading style={styles.center}>
        {title}
      </Text>
      {body ? (
        <Text variant="callout" tone="secondary" style={styles.center}>
          {body}
        </Text>
      ) : null}
      {action ? <Button title={action.title} onPress={action.onPress} block={false} style={styles.action} /> : null}
    </View>
  );
}

const styles = StyleSheet.create({
  wrap: { flex: 1, alignItems: 'center', justifyContent: 'center', gap: 10 },
  center: { textAlign: 'center' },
  action: { alignSelf: 'center', marginTop: 8 },
});
