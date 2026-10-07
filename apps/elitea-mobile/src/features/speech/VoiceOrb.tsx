import { useEffect, useState } from 'react';
import { Animated, Easing, StyleSheet, View } from 'react-native';
import Svg, { Circle, Defs, LinearGradient, RadialGradient, Stop } from 'react-native-svg';

import { useDesign } from '@/design/DesignProvider';

/**
 * The voice orb of the reference (docs/design-system.md §4.8): a glossy sphere in the orb stops
 * (pink → lavender → mint, the corrected workspace accent at the first stop when the workspace
 * is branded) with a specular highlight. Decorative: the listening state is said in text next
 * to it, never by the orb alone.
 *
 * While `active` it breathes (a slow 2.4 s scale loop) and grows with the microphone level
 * (1.0 → 1.12). Reduce Motion: no breathing and no scaling; a static ring around the orb steps
 * its width with the level instead (§1.9).
 */
export function VoiceOrb({ size, active, level }: { size: number; active: boolean; level: number }) {
  const design = useDesign();
  const { colors, motion, reduceMotion } = design;
  const stops = colors.orb.stops;
  const first = design.branded ? colors.accent : stops[0];
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
          <Defs>
            <LinearGradient id="orbFill" x1="0.15" y1="0.1" x2="0.85" y2="0.95">
              <Stop offset="0" stopColor={first} />
              <Stop offset="0.55" stopColor={stops[1]} />
              <Stop offset="1" stopColor={stops[2]} />
            </LinearGradient>
            <RadialGradient id="orbShine" cx="0.36" cy="0.3" rx="0.42" ry="0.36" fx="0.34" fy="0.26">
              <Stop offset="0" stopColor="#FFFFFF" stopOpacity={0.95} />
              <Stop offset="0.45" stopColor={colors.orb.highlight} stopOpacity={0.35} />
              <Stop offset="1" stopColor="#FFFFFF" stopOpacity={0} />
            </RadialGradient>
            <RadialGradient id="orbRim" cx="0.5" cy="0.5" r="0.5">
              <Stop offset="0.78" stopColor="#FFFFFF" stopOpacity={0} />
              <Stop offset="1" stopColor="#FFFFFF" stopOpacity={design.scheme === 'dark' ? 0.25 : 0.55} />
            </RadialGradient>
          </Defs>
          <Circle cx="50" cy="50" r="49" fill="url(#orbFill)" />
          <Circle cx="50" cy="50" r="49" fill="url(#orbRim)" />
          <Circle cx="50" cy="50" r="49" fill="url(#orbShine)" />
        </Svg>
      </Animated.View>
    </View>
  );
}

const styles = StyleSheet.create({
  box: { alignItems: 'center', justifyContent: 'center' },
  ring: { opacity: 0.6 },
});
