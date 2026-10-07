import type { ReactNode } from 'react';
import { ActivityIndicator, StyleSheet, Text, View } from 'react-native';

import { MaxContentWidth, Spacing } from '@/constants/theme';
import { usePalette } from '@/constants/use-palette';
import { useBrandFontFamily } from '@/theme/brandFonts';
import { ramp } from '@/ui';

/** Full-screen neutral state: launch, "Signing out…", blocking notices. */
export function StatusScreen({
  title,
  body,
  busy = false,
  children,
}: {
  title?: string;
  body?: string;
  busy?: boolean;
  children?: ReactNode;
}) {
  const palette = usePalette();
  const fontFamily = useBrandFontFamily();
  return (
    <View style={[styles.container, { backgroundColor: palette.background }]}>
      <View style={styles.content}>
        {busy ? <ActivityIndicator color={palette.accent} /> : null}
        {title ? (
          <Text accessibilityRole="header" style={[styles.title, { color: palette.text, fontFamily }]}>
            {title}
          </Text>
        ) : null}
        {body ? <Text style={[styles.body, { color: palette.textSecondary }]}>{body}</Text> : null}
        {children}
      </View>
    </View>
  );
}

const styles = StyleSheet.create({
  container: { flex: 1, alignItems: 'center', justifyContent: 'center', padding: Spacing.four },
  content: { width: '100%', maxWidth: MaxContentWidth, gap: Spacing.three, alignItems: 'center' },
  title: { ...ramp.title2, fontWeight: '700', textAlign: 'center' },
  body: { ...ramp.body, textAlign: 'center' },
});
