import { MaterialSymbolsRounded_400Regular } from '@expo-google-fonts/material-symbols-rounded/400Regular';
/** The weight object expo-symbols takes for Android (`{ name, font }`). */
export type AndroidSymbolWeight = { name: string; font: number };

/** Material Symbols Rounded 400 (docs/design-system.md §1.8): rounded matches the brand. */
export const MATERIAL_WEIGHT: AndroidSymbolWeight | null = {
  name: 'MaterialSymbolsRounded_400Regular',
  font: MaterialSymbolsRounded_400Regular,
};
