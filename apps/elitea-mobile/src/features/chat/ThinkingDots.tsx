import { useEffect, useState } from 'react';
import { Animated, Easing, StyleSheet, View } from 'react-native';

import { useDesign } from '@/design/DesignProvider';

/**
 * The typing indicator of a reply that has not written anything yet (docs/design-system.md
 * §4.6): three dots that bob one after another (a bouncy loop), static dots under Reduce
 * Motion. Decorative; the bubble's text says "Thinking…" for screen readers.
 */
export function ThinkingDots() {
  const design = useDesign();
  const [dots] = useState(() => [0, 1, 2].map(() => new Animated.Value(0)));
  useEffect(() => {
    if (design.reduceMotion) {
      for (const dot of dots) dot.setValue(0);
      return;
    }
    const step = design.motion.duration.base;
    const loop = Animated.loop(
      Animated.stagger(
        step / 2,
        dots.map((dot) =>
          Animated.sequence([
            Animated.timing(dot, { toValue: 1, duration: step, easing: Easing.out(Easing.quad), useNativeDriver: true }),
            Animated.timing(dot, { toValue: 0, duration: step, easing: Easing.in(Easing.quad), useNativeDriver: true }),
            Animated.delay(step),
          ]),
        ),
      ),
    );
    loop.start();
    return () => loop.stop();
  }, [design.reduceMotion, design.motion.duration.base, dots]);
  return (
    <View accessible={false} importantForAccessibility="no-hide-descendants" accessibilityElementsHidden testID="thinking-dots" style={styles.row}>
      {dots.map((dot, index) => (
        <Animated.View
          key={index}
          style={[
            styles.dot,
            {
              backgroundColor: design.colors.textTertiary,
              opacity: dot.interpolate({ inputRange: [0, 1], outputRange: [0.45, 1] }),
              transform: [{ translateY: dot.interpolate({ inputRange: [0, 1], outputRange: [0, -4] }) }],
            },
          ]}
        />
      ))}
    </View>
  );
}

const styles = StyleSheet.create({
  row: { flexDirection: 'row', alignItems: 'center', gap: 5, height: 22, paddingHorizontal: 2 },
  dot: { width: 8, height: 8, borderRadius: 4 },
});
