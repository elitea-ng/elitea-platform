import { LinearGradient } from 'expo-linear-gradient';
import { StyleSheet, View, type ViewProps } from 'react-native';

import { useDesign } from '@/design/DesignProvider';
import type { Gradient } from '@/design/tokens';

/** Start/end points of a CSS-style angle (0° = up, 90° = right) on the unit square. */
export function gradientPoints(angleDeg: number): { start: { x: number; y: number }; end: { x: number; y: number } } {
  const rad = (angleDeg * Math.PI) / 180;
  const dx = Math.sin(rad) / 2;
  const dy = -Math.cos(rad) / 2;
  const round = (n: number) => Math.round(n * 1000) / 1000;
  return { start: { x: round(0.5 - dx), y: round(0.5 - dy) }, end: { x: round(0.5 + dx), y: round(0.5 + dy) } };
}

/**
 * The pastel ground of Home, connect, sign-in, voice and empty states (§1.2): one full-screen
 * gradient that does not scroll. Chat and long reading surfaces use the flat `background`.
 * Reduce Transparency / Increase Contrast draw the flat background instead.
 */
export function GradientBackground({ style, children, gradient, ...rest }: ViewProps & { gradient?: Gradient }) {
  const design = useDesign();
  const g = gradient ?? design.colors.backgroundGradient;
  if (!design.glass) {
    return (
      <View style={[styles.fill, { backgroundColor: design.colors.background }, style]} {...rest}>
        {children}
      </View>
    );
  }
  const { start, end } = gradientPoints(g.angleDeg);
  return (
    <LinearGradient colors={[...g.stops]} locations={[...g.positions]} start={start} end={end} style={[styles.fill, style]} {...rest}>
      {children}
    </LinearGradient>
  );
}

const styles = StyleSheet.create({ fill: { flex: 1 } });
