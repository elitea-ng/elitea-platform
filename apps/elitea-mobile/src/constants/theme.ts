/**
 * Fallback palette. At runtime this is overridden by the connected server's
 * brand pack (see docs/design.md "Runtime branding") — TODO.
 */
export const Colors = {
  light: {
    text: '#000000',
    textSecondary: '#60646C',
    background: '#ffffff',
    backgroundElement: '#F0F0F3',
    border: '#D9D9E0',
    accent: '#208AEF',
    onAccent: '#ffffff',
    danger: '#D93025',
  },
  dark: {
    text: '#ffffff',
    textSecondary: '#B0B4BA',
    background: '#000000',
    backgroundElement: '#212225',
    border: '#3A3D42',
    accent: '#4AA3F5',
    onAccent: '#000000',
    danger: '#FF6B5E',
  },
} as const;

export type Palette = (typeof Colors)['light' | 'dark'];

export const Spacing = {
  one: 4,
  two: 8,
  three: 16,
  four: 24,
  five: 32,
} as const;

export const MaxContentWidth = 640;
