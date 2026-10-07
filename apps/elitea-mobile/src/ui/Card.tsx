import { useState } from 'react';
import { Animated, Pressable, StyleSheet, View, type PressableProps, type ViewProps } from 'react-native';

import { useDesign } from '@/design/DesignProvider';

import { GlassSurface } from './GlassSurface';
import { Icon } from './Icon';
import type { IconName } from './icons';
import { ripple } from './platform';
import { Text } from './Text';

/** A content card (§1.5 radius 22, padding 16): `surface` on flat grounds, glass on the gradient. */
export function Card({ glass = false, style, children, ...rest }: ViewProps & { glass?: boolean }) {
  const design = useDesign();
  const frame = [styles.card, { padding: design.space.cardPadding, borderRadius: design.radius.card }];
  if (glass) {
    return (
      <GlassSurface radius={design.radius.card} style={[frame, style]} {...rest}>
        {children}
      </GlassSurface>
    );
  }
  return (
    <View
      style={[
        frame,
        { backgroundColor: design.colors.surface, borderColor: design.colors.separator, borderWidth: design.scheme === 'light' ? StyleSheet.hairlineWidth : 0 },
        design.scheme === 'light' ? design.elevation.e1 : null,
        style,
      ]}
      {...rest}>
      {children}
    </View>
  );
}

/**
 * The reference's circular arrow button, as a VISUAL only (§4.2): the card or row it sits on is
 * the one button, so this circle is hidden from assistive technology and takes no taps.
 */
export function ArrowButton({ size = 40, icon = 'arrowGo', style }: { size?: number; icon?: IconName; style?: ViewProps['style'] }) {
  const { colors, scheme } = useDesign();
  return (
    <View
      accessible={false}
      importantForAccessibility="no-hide-descendants"
      accessibilityElementsHidden
      pointerEvents="none"
      style={[
        styles.arrow,
        { width: size, height: size, borderRadius: size / 2, borderColor: colors.separator, backgroundColor: scheme === 'dark' ? colors.elevated : colors.surface },
        style,
      ]}>
      <Icon name={icon} size={Math.round(size * 0.45)} tone="primary" weight="semibold" />
    </View>
  );
}

/** A 40 pt icon tile (`accentSoft` circle, icon in `accentSoftOn`). */
export function IconTile({ icon, size = 40, tone = 'accent' }: { icon: IconName; size?: number; tone?: 'accent' | 'secondary' }) {
  const { colors } = useDesign();
  return (
    <View
      accessible={false}
      importantForAccessibility="no-hide-descendants"
      accessibilityElementsHidden
      style={[styles.tile, { width: size, height: size, borderRadius: size / 2, backgroundColor: tone === 'accent' ? colors.accentSoft : colors.secondarySoft }]}>
      <Icon name={icon} size={Math.round(size * 0.5)} color={tone === 'accent' ? colors.accentSoftOn : colors.secondarySoftOn} />
    </View>
  );
}

export interface ActionCardProps extends Omit<PressableProps, 'children' | 'style'> {
  title: string;
  icon: IconName;
  /** What tapping does, for screen readers ("Starts a new chat"). */
  accessibilityHint: string;
  style?: ViewProps['style'];
}

/**
 * The action card of the reference (§4.2): a glass card, radius 28, min height 132, with an icon
 * tile top-start, the circular arrow top-end and the title at the bottom. The whole card is one
 * button; pressed state is scale .98 (spring) plus the overlay, overlay only with Reduce Motion.
 */
export function ActionCard({ title, icon, accessibilityHint, disabled, style, ...rest }: ActionCardProps) {
  const design = useDesign();
  const [scale] = useState(() => new Animated.Value(1));
  const press = (to: number) => {
    if (design.reduceMotion) return;
    const { damping, stiffness, mass } = design.motion.spring.snappy;
    Animated.spring(scale, { toValue: to, damping, stiffness, mass, useNativeDriver: true }).start();
  };
  return (
    <Animated.View style={[styles.actionWrap, { transform: [{ scale }] }, style]}>
      <Pressable
        accessibilityRole="button"
        accessibilityLabel={title}
        accessibilityHint={accessibilityHint}
        accessibilityState={{ disabled: disabled === true }}
        disabled={disabled}
        android_ripple={ripple(design)}
        onPressIn={() => press(0.98)}
        onPressOut={() => press(1)}
        style={({ pressed }) => [styles.flex, pressed ? styles.pressed : null, disabled ? styles.disabled : null]}
        {...rest}>
        <GlassSurface radius={design.radius.actionCard} style={[styles.action, { padding: design.space.cardPadding }]}>
          <View style={styles.actionTop}>
            <IconTile icon={icon} />
            <ArrowButton />
          </View>
          <Text variant="headline" numberOfLines={design.accessibilityLayout ? 3 : 2}>
            {title}
          </Text>
        </GlassSurface>
      </Pressable>
    </Animated.View>
  );
}

const styles = StyleSheet.create({
  card: { borderCurve: 'continuous', gap: 8 },
  arrow: { alignItems: 'center', justifyContent: 'center', borderWidth: StyleSheet.hairlineWidth },
  tile: { alignItems: 'center', justifyContent: 'center' },
  actionWrap: { flex: 1, minWidth: 140 },
  flex: { flex: 1 },
  action: { flex: 1, minHeight: 132, justifyContent: 'space-between', gap: 12 },
  actionTop: { flexDirection: 'row', justifyContent: 'space-between', alignItems: 'flex-start' },
  pressed: { opacity: 0.85 },
  disabled: { opacity: 0.38 },
});
