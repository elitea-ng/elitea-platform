import { useEffect, useState, type ReactNode } from 'react';
import { StyleSheet, View } from 'react-native';
import Animated, {
  Easing,
  cancelAnimation,
  useAnimatedStyle,
  useSharedValue,
  withRepeat,
  withSequence,
  withTiming,
} from 'react-native-reanimated';

import { MOTION_KIND, MOTION_SPEC, MOTION_TONE, nextChangeAt, type AvatarMotion, type AvatarStates, type MotionTone } from '@elitea/client-core/conversation';

import { useDesign } from '@/design/DesignProvider';
import type { Design } from '@/design/resolve';

/**
 * The default motion pack over an agent's avatar (ADR-0050 decision 6): one
 * Reanimated motion per state from `MOTION_SPEC`
 * (`@elitea/client-core/conversation`), drawn around the still. The web and
 * the desktop play the same spec in CSS.
 *
 * Colours are design tokens (the workspace's corrected brand accent and the
 * signal colours), so a white label re-themes the pack. `motion="still"`
 * draws the avatar alone; the thread decides it (one animated avatar per
 * list, Reduce Motion, data-saver). Decorative: the bubble says who answers.
 */
export function AgentMotion({ motion, size, children }: { motion: AvatarMotion; size: number; children: ReactNode }) {
  const design = useDesign();
  const spec = motion === 'still' || design.reduceMotion ? undefined : MOTION_SPEC[motion];
  const progress = useSharedValue(0);

  useEffect(() => {
    cancelAnimation(progress);
    progress.value = 0;
    if (spec === undefined) return;
    const step = withTiming(1, { duration: spec.durationMs, easing: Easing.inOut(Easing.quad) });
    progress.value = spec.loop ? withRepeat(withSequence(step, withTiming(0, { duration: 0 })), -1) : step;
    return () => cancelAnimation(progress);
  }, [spec, progress]);

  const kind = spec?.kind;
  const peak = spec?.peakScale ?? 1;
  const ringStyle = useAnimatedStyle(() => {
    const p = progress.value;
    switch (kind) {
      case MOTION_KIND.Ring:
      case MOTION_KIND.Orbit:
        return { transform: [{ rotate: `${p * 360}deg` }], opacity: 1 };
      case MOTION_KIND.Pulse:
        return { transform: [{ scale: 1 + (Math.max(peak, 1.35) - 1) * p }], opacity: 0.9 * (1 - p) };
      case MOTION_KIND.Halo:
        return { transform: [{ scale: 1 }], opacity: 0.35 + 0.65 * Math.sin(Math.PI * p) };
      case MOTION_KIND.Badge:
        return { transform: [{ scale: p < 0.7 ? (p / 0.7) * 1.2 : 1.2 - ((p - 0.7) / 0.3) * 0.2 }], opacity: 1 };
      default:
        return { transform: [{ scale: 1 }], opacity: 0 };
    }
  });
  const stillStyle = useAnimatedStyle(() => {
    const breathe = kind === MOTION_KIND.Breathe ? 1 + (peak - 1) * Math.sin(Math.PI * progress.value) : 1;
    return { transform: [{ scale: breathe }] };
  });

  const color = toneColor(design, spec?.tone);
  const box = { width: size, height: size };
  const ringBox = { width: size + 6, height: size + 6, borderRadius: (size + 6) / 2, left: -3, top: -3 };
  return (
    <View testID="agent-motion" accessibilityLabel={undefined} accessible={false} importantForAccessibility="no-hide-descendants" style={box}>
      <Animated.View style={[box, stillStyle]}>{children}</Animated.View>
      {spec !== undefined && kind !== MOTION_KIND.Breathe && (
        <Animated.View
          testID={`agent-motion-${motion}`}
          pointerEvents="none"
          style={[
            styles.layer,
            kind === MOTION_KIND.Badge ? [styles.badge, { backgroundColor: color, borderColor: design.colors.background }] : ringBox,
            kind === MOTION_KIND.Ring && { borderWidth: 2, borderColor: 'transparent', borderTopColor: color, borderRightColor: color },
            (kind === MOTION_KIND.Pulse || kind === MOTION_KIND.Halo) && { borderWidth: 2, borderColor: color },
            ringStyle,
          ]}
        >
          {kind === MOTION_KIND.Orbit && <View style={[styles.dot, { backgroundColor: color, left: (size + 6) / 2 - 2.5 }]} />}
        </Animated.View>
      )}
    </View>
  );
}

function toneColor(design: Design, tone: MotionTone | undefined): string {
  switch (tone) {
    case MOTION_TONE.Attention:
      return design.colors.warning;
    case MOTION_TONE.Success:
      return design.colors.success;
    case MOTION_TONE.Error:
      return design.colors.danger;
    case MOTION_TONE.Accent:
    case undefined:
      return design.colors.accent;
  }
}

const styles = StyleSheet.create({
  layer: { position: 'absolute' },
  badge: { right: -3, bottom: -3, width: 12, height: 12, borderRadius: 6, borderWidth: 1 },
  dot: { position: 'absolute', top: 0, width: 5, height: 5, borderRadius: 2.5 },
});

/**
 * The clock the avatar state machine reads: it moves when the states change
 * and when a `success` / `error` slot ends on its own (`nextChangeAt`).
 * A state change stamps its own `since`, so reading the newest `since` as
 * "now" is exact until the timer moves the clock on.
 */
export function useAvatarClock(states: AvatarStates): number {
  const latest = Object.values(states).reduce((max, entry) => Math.max(max, entry.since), 0);
  const [tick, setTick] = useState<{ after: number; now: number }>({ after: 0, now: 0 });
  const now = tick.after === latest ? Math.max(tick.now, latest) : latest;
  useEffect(() => {
    const at = nextChangeAt(states, now);
    if (at === undefined) return;
    const timer = setTimeout(() => setTick({ after: latest, now: at }), Math.max(0, at - now));
    return () => clearTimeout(timer);
  }, [states, now, latest]);
  return now;
}
