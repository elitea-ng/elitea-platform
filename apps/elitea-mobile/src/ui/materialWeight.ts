/** The weight object expo-symbols takes for Android (`{ name, font }`). */
export type AndroidSymbolWeight = { name: string; font: number };

/**
 * The Material Symbols face for Android: Rounded, weight 400 (docs/design-system.md §1.8).
 * iOS draws SF Symbols and needs no font, so this file is the iOS/web stub; the Android
 * build resolves `materialWeight.android.ts`, which bundles the font.
 */
export const MATERIAL_WEIGHT: AndroidSymbolWeight | null = null;
