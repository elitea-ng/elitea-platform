import { useEffect, useState } from 'react';
import { Animated, Easing, StyleSheet, View } from 'react-native';
import Svg, { Circle } from 'react-native-svg';

import { useDesign } from '@/design/DesignProvider';

/**
 * The voice orb (docs/design-system.md §4.8): a flat disc in the orb colour (the scheme's signal
 * colour; the corrected workspace accent when the workspace is branded). No gradient, no
 * highlight (brand guide v3). Decorative: the listening state is said in text next to it, never
 * by the orb alone.
 *
 * While `active` it breathes (a slow 2.4 s scale loop) and grows with the microphone level
 * (1.0 → 1.12). Reduce Motion: no breathing and no scaling; a static ring around the orb steps
 * its width with the level instead (§1.9).
 */
export function VoiceOrb({ size, active, level }: { size: number; active: boolean; level: number }) {
  const design = useDesign();
  const { colors, motion, reduceMotion } = design;
  const first = design.branded ? colors.accent : colors.orb.stops[0];
  const [breath] = useState(() => new Animated.Value(0));
  const [loud] = useState(() => new Animated.Value(0));

  useEffect(() => {
    if (!active || reduceMotion) {
      breath.setValue(0);
      return;
    }
    const half = motion.duration.orbBreath / 2;
    const loop = Animated.loop(
      Animated.sequence([
        Animated.timing(breath, { toValue: 1, duration: half, easing: Easing.inOut(Easing.sin), useNativeDriver: true }),
        Animated.timing(breath, { toValue: 0, duration: half, easing: Easing.inOut(Easing.sin), useNativeDriver: true }),
      ]),
    );
    loop.start();
    return () => loop.stop();
  }, [active, reduceMotion, breath, motion.duration.orbBreath]);

  useEffect(() => {
    if (reduceMotion) return;
    Animated.spring(loud, { toValue: active ? level : 0, ...motion.spring.bouncy, useNativeDriver: true }).start();
  }, [level, active, reduceMotion, loud, motion.spring.bouncy]);

  const scale = Animated.add(
    breath.interpolate({ inputRange: [0, 1], outputRange: [1, 1.04] }),
    loud.interpolate({ inputRange: [0, 1], outputRange: [0, 0.08], extrapolate: 'clamp' }),
  );
  // Reduce Motion: a ring of 0-3 steps (2 pt each) instead of any movement.
  const ring = reduceMotion && active ? Math.min(3, Math.round(level * 3)) * 2 : 0;
  return (
    <View
      accessible={false}
      importantForAccessibility="no-hide-descendants"
      accessibilityElementsHidden
      testID="voice-orb"
      style={[styles.box, { width: size + 12, height: size + 12 }]}>
      {ring > 0 ? (
        <View
          testID="voice-orb-ring"
          style={[StyleSheet.absoluteFill, styles.ring, { borderRadius: (size + 12) / 2, borderWidth: ring, borderColor: first }]}
        />
      ) : null}
      <Animated.View style={{ width: size, height: size, transform: [{ scale }] }}>
        <Svg width={size} height={size} viewBox="0 0 100 100">
          <Circle cx="50" cy="50" r="49" fill={first} />
        </Svg>
      </Animated.View>
    </View>
  );
}

const styles = StyleSheet.create({
  box: { alignItems: 'center', justifyContent: 'center' },
  ring: { opacity: 0.6 },
});
