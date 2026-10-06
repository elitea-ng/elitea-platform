/** The Agent Zefir design system (docs/design-system.md). Screens use `useDesign()`. */
export { DesignProvider, useAccessibilityModifiers, useDesign } from './DesignProvider';
export { brandOverrides, correctAccent, resolveBrand, type BrandInput, type BrandOverrides } from './brand';
export { accentGrounds, minContrast, proofRatio } from './contrast';
export { resolveDesign, type AccessibilityModifiers, type Design, type SchemePreference } from './resolve';
export * from './tokens';
export { typography, ACCESSIBILITY_LAYOUT_SCALE, type TypeRole, type TypeStyle, type Typography } from './typography';
