import { View } from 'react-native';
import Svg, { Path } from 'react-native-svg';

import { DEFAULT_AVATAR_GRID, defaultAvatarOf, type DefaultAvatarRole } from '@elitea/client-core/conversation';

import { BrandMark } from '@/components/BrandMark';
import { useDesign } from '@/design/DesignProvider';

/**
 * The image of a sub-agent profile's default avatar key (`default:<name>`,
 * issue 1870), drawn from the shared set in `@elitea/client-core/conversation`
 * as the web and the desktop draw it. A shape's role maps to a design token,
 * so a white-label brand and the dark scheme re-theme it. An unknown key
 * draws the brand mark. Decorative:
 * the group's label names the sub-agent.
 */
export function DefaultAvatarGlyph({ avatarKey, size = 24 }: { avatarKey: string | null | undefined; size?: number }) {
  const { colors } = useDesign();
  const avatar = defaultAvatarOf(avatarKey);
  if (avatar === undefined) return <BrandMark variant="avatar" size={size} decorative testID="brand-mark-fallback" />;
  const fill: Record<DefaultAvatarRole, string> = { body: colors.accent, detail: colors.accentOn, accent: colors.textSecondary };
  return (
    <View testID="default-avatar" accessible={false} importantForAccessibility="no-hide-descendants" accessibilityElementsHidden>
      <Svg width={size} height={size} viewBox={`0 0 ${DEFAULT_AVATAR_GRID} ${DEFAULT_AVATAR_GRID}`}>
        {avatar.shapes.map((shape, index) => (
          <Path key={index} d={shape.d} fill={fill[shape.role]} />
        ))}
      </Svg>
    </View>
  );
}
