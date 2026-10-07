import type { ReactNode } from 'react';
import { ActivityIndicator, StyleSheet, View } from 'react-native';

import { Mascot } from '@/components/Mascot';
import { MaxContentWidth } from '@/constants/theme';
import { useDesign } from '@/design/DesignProvider';
import { GradientBackground, Text } from '@/ui';

/**
 * Full-screen neutral state: launch, "Signing out…", blocking notices. `mascot` adds Agent
 * Zefirca above the title on the gradient ground (an update, a friendly blocking state); never
 * for errors that lose data, security or approval states (design-system §5).
 */
export function StatusScreen({
  title,
  body,
  busy = false,
  mascot,
  children,
}: {
  title?: string;
  body?: string;
  busy?: boolean;
  mascot?: 'bust' | 'full';
  children?: ReactNode;
}) {
  const design = useDesign();
  const content = (
    <View style={[styles.container, { padding: design.space.screenGutter }]}>
      <View style={styles.content}>
        {mascot ? (
          <Mascot variant={design.accessibilityLayout ? 'bust' : mascot} size={mascot === 'full' && !design.accessibilityLayout ? 140 : 96} decorative />
        ) : null}
        {busy ? <ActivityIndicator size="large" color={design.colors.accent} /> : null}
        {title ? (
          <Text variant="title2" heading style={styles.center}>
            {title}
          </Text>
        ) : null}
        {body ? (
          <Text variant="body" tone="secondary" style={styles.center}>
            {body}
          </Text>
        ) : null}
        {children}
      </View>
    </View>
  );
  if (mascot) return <GradientBackground>{content}</GradientBackground>;
  return <View style={[styles.flex, { backgroundColor: design.colors.background }]}>{content}</View>;
}

const styles = StyleSheet.create({
  flex: { flex: 1 },
  container: { flex: 1, alignItems: 'center', justifyContent: 'center' },
  content: { width: '100%', maxWidth: MaxContentWidth, gap: 14, alignItems: 'center' },
  center: { textAlign: 'center' },
});
