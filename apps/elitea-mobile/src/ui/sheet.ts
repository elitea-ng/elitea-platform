import type { NativeStackNavigationOptions } from 'expo-router';

import type { Design } from '@/design/resolve';

/** Sheet detents (§4.10): a half sheet that can grow to full, or one that fits its content. */
export type SheetSize = 'half' | 'large' | 'fit';

const DETENTS: Record<SheetSize, NonNullable<NativeStackNavigationOptions['sheetAllowedDetents']>> = {
  half: [0.5, 1],
  large: [1],
  fit: 'fitToContents',
};

/**
 * A native sheet route (§4.10, audit X9/X10): `formSheet` with detents and a grabber on iOS
 * (the system's corner radius and glass), a Material 3 modal bottom sheet on Android. The route
 * gets a native header, so Cancel/Done are bar buttons and navigation inside the sheet is a
 * push, not a mode switch. Swipe-down, the grabber and Android back all dismiss it.
 */
export function sheetOptions(design: Pick<Design, 'colors'>, size: SheetSize = 'half'): NativeStackNavigationOptions {
  return {
    presentation: 'formSheet',
    sheetAllowedDetents: DETENTS[size],
    sheetGrabberVisible: true,
    sheetCornerRadius: 32,
    sheetExpandsWhenScrolledToEdge: true,
    contentStyle: { backgroundColor: design.colors.elevated },
    headerStyle: { backgroundColor: design.colors.elevated },
  };
}
