import { GlassView, isLiquidGlassAvailable } from 'expo-glass-effect';
import { StyleSheet, View, type ViewProps } from 'react-native';

import { useDesign } from '@/design/DesignProvider';

import { glassFill, glassMode } from './glass';
import { uiPlatform } from './platform';

let liquid: boolean | null = null;
function liquidAvailable(): boolean {
  if (liquid === null) {
    try {
      liquid = isLiquidGlassAvailable();
    } catch {
      liquid = false;
    }
  }
  return liquid;
}

export interface GlassSurfaceProps extends ViewProps {
  /** Corner radius; glass is always rounded (continuous curve on iOS). */
  radius?: number;
  /** Interactive glass reacts to touches (iOS 26: for controls such as the composer). */
  interactive?: boolean;
  /** Raise it with the e2 shadow (floating composer, FAB, popovers). */
  floating?: boolean;
}

/**
 * A glass surface (§1.3): Liquid Glass on iOS 26+, a tonal surface on Android and older iOS,
 * opaque under Reduce Transparency / Increase Contrast. Text on it is proven against the worst
 * composite, so content is legible in every mode. The Android shadow sits on an opaque view
 * only (a translucent one would show the shadow through the glass).
 */
export function GlassSurface({ radius, interactive = false, floating = false, style, children, ...rest }: GlassSurfaceProps) {
  const design = useDesign();
  const glass = design.colors.surfaceGlass;
  const mode = glassMode(uiPlatform, liquidAvailable(), design.glass);
  const r = radius ?? design.radius.card;
  const shape = { borderRadius: r, borderCurve: 'continuous' as const };
  const shadow = floating ? design.elevation.e2 : null;
  if (mode === 'liquid') {
    return (
      <GlassView
        glassEffectStyle="regular"
        isInteractive={interactive}
        colorScheme={design.scheme}
        // The system glass draws its own depth; a shadow on it cannot be rasterised efficiently.
        style={[shape, style]}
        {...rest}>
        {children}
      </GlassView>
    );
  }
  return (
    <View
      style={[
        shape,
        { backgroundColor: glassFill(glass, mode), borderWidth: StyleSheet.hairlineWidth, borderColor: design.scheme === 'dark' ? glass.border : design.colors.separator },
        shadow && (mode === 'opaque' || uiPlatform === 'ios' ? shadow : { ...shadow, elevation: 0 }),
        style,
      ]}
      {...rest}>
      {children}
    </View>
  );
}
