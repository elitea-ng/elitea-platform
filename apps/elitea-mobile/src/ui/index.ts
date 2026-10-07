/**
 * The Agent Zefir component kit (docs/design-system.md §4). Every screen builds from these and
 * reads tokens through `useDesign()`; nothing here hard-codes a colour, a font size or a target.
 */
export { Avatar, AVATAR_TONES, initialsOf, toneIndex, type AvatarProps } from './Avatar';
export { Badge, badgeText } from './Badge';
export { Button, buttonHeight, buttonLook, type ButtonProps, type ButtonSize, type ButtonVariant } from './Button';
export { ActionCard, ArrowButton, Card, IconTile, type ActionCardProps } from './Card';
export { Chip, ChipRow, type ChipProps } from './Chip';
export { EmptyState, type EmptyStateProps } from './EmptyState';
export { glassFill, glassMode, withAlpha, type GlassMode } from './glass';
export { GlassSurface, type GlassSurfaceProps } from './GlassSurface';
export { GradientBackground, gradientPoints } from './GradientBackground';
export { haptic, type HapticEvent } from './haptics';
export { headerActionOptions, iosMajor, largeTitleOptions, nativeHeaderItemLabel, searchBarOptions, type HeaderAction } from './header';
export { Icon, type IconProps } from './Icon';
export { filledSymbol, ICONS, symbolFor, type IconName } from './icons';
export { IconButton, type IconButtonProps, type IconButtonVariant } from './IconButton';
export { layoutMargin, useLayoutMargin } from './layout';
export { ListRow, ListSection, sectionTitle, type ListRowProps } from './List';
export { hitSlopFor, hitTarget, pressedOverlay, ripple, uiPlatform, type UiPlatform } from './platform';
export { SegmentedControl, selectedSegmentStyle, type Segment } from './SegmentedControl';
export { sheetOptions, type SheetSize } from './sheet';
export { nativeTabsProps } from './tabs';
export { Text, toneColor, type TextProps, type TextTone } from './Text';
export { TextField, type TextFieldProps } from './TextField';
export { maxScale, maxScales, monogramStyle, ramp, textStyles, type TypeRole } from './type';
