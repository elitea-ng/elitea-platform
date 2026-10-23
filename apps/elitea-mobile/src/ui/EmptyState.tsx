import { StyleSheet, View, type ViewProps } from 'react-native';

import { EliteaMark } from '@/components/EliteaMark';
import { useDesign } from '@/design/DesignProvider';

import { Button } from './Button';
import { Text } from './Text';

export interface EmptyStateProps {
  title: string;
  body?: string;
  /** One primary action (a filled pill). */
  action?: { title: string; onPress: () => void };
  /**
   * The Elitea mark (design-system §4.9, §5): `large` (96) in roomy containers, `small` (64) in
   * short ones, `none` for errors that involve data loss, security or approval states.
   */
  mark?: 'large' | 'small' | 'none';
  style?: ViewProps['style'];
}

/**
 * The empty state (§4.9): a centred column with the Elitea mark (decorative: the title carries the
 * meaning), a title2 heading, one callout line and one primary action. At accessibility text
 * sizes the mark drops to the small size so the text keeps the room.
 */
export function EmptyState({ title, body, action, mark = 'large', style }: EmptyStateProps) {
  const design = useDesign();
  const variant = mark === 'none' ? null : design.accessibilityLayout ? 'small' : mark;
  return (
    <View style={[styles.wrap, { padding: design.space.screenGutter }, style]}>
      {variant ? <EliteaMark size={variant === 'large' ? 96 : 64} decorative testID="elitea-mark" /> : null}
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
