import { HStack, Image, Spacer, Text, VStack } from '@expo/ui/swift-ui';
import { activityBackgroundTint, font, foregroundStyle, frame, lineLimit, monospacedDigit, multilineTextAlignment, padding } from '@expo/ui/swift-ui/modifiers';
import { createLiveActivity, type LiveActivityEnvironment } from 'expo-widgets';

import type { TurnActivityProps } from '@/features/surfaces/turnActivityModel';

/**
 * The Live Activity (lock screen and Dynamic Island) of a running answer;
 * see src/features/surfaces/turnActivityModel.ts for what it may show.
 * Rendered in the widget extension's runtime: only `@expo/ui/swift-ui`
 * views and the props (the 'widget' directive serialises the function).
 */
export const TURN_ACTIVITY = 'ZefirTurn';

const TurnActivityLayout = (props: TurnActivityProps, environment: LiveActivityEnvironment) => {
  'widget';
  // Zefir rose per scheme (design-tokens.json accent: light #B4306A, dark #FF8CB8); white on the
  // dimmed Always-On lock screen. Grounds are the app's own: cream (light) and night surface (dark).
  const dark = environment.colorScheme === 'dark';
  const accent = environment.isLuminanceReduced ? '#FFFFFF' : dark ? '#FF8CB8' : '#B4306A';
  const ground = dark ? '#1D1A22' : '#FBF7F9';
  const symbols = {
    working: 'sparkles',
    tool: 'wrench.and.screwdriver.fill',
    writing: 'text.bubble.fill',
    waiting: 'hand.raised.fill',
    done: 'checkmark.circle.fill',
    failed: 'exclamationmark.triangle.fill',
    stopped: 'stop.circle.fill',
  } as const;
  const symbol = symbols[props.phase] ?? 'sparkles';
  const muted = foregroundStyle({ type: 'hierarchical', style: 'secondary' });
  // The elapsed time is a SwiftUI timer counting up from the start: the system
  // advances it, while the activity is only updated when what it shows
  // changes. A finished or paused one shows none (it would keep counting).
  const running = props.phase === 'working' || props.phase === 'tool' || props.phase === 'writing';
  // A SwiftUI timer Text takes all the width it is offered (it sizes for the longest time it
  // could show), so beside the title it split the banner in half and cut the title short (#41).
  // A fixed trailing frame keeps it to the digits; the title gets the rest.
  const elapsed = (style: ReturnType<typeof font>[], width?: number) =>
    running ? (
      <Text
        date={new Date(props.startedAt)}
        dateStyle="timer"
        modifiers={[...style, monospacedDigit(), multilineTextAlignment('trailing'), ...(width ? [frame({ width, alignment: 'trailing' })] : [])]}
      />
    ) : null;
  return {
    banner: (
      <HStack spacing={12} modifiers={[padding({ all: 14 }), activityBackgroundTint(ground)]}>
        <Image systemName={symbol} color={accent} size={24} />
        <VStack alignment="leading" spacing={2}>
          <Text modifiers={[font({ textStyle: 'headline', design: 'rounded' }), lineLimit(1)]}>{props.title}</Text>
          <Text modifiers={[font({ textStyle: 'subheadline' }), muted, lineLimit(1)]}>{props.detail}</Text>
        </VStack>
        <Spacer />
        {elapsed([font({ textStyle: 'caption' }), muted], 56)}
      </HStack>
    ),
    compactLeading: <Image systemName={symbol} color={accent} />,
    compactTrailing: props.approximate ? (
      <Text modifiers={[font({ textStyle: 'caption' })]}>…</Text>
    ) : (
      (elapsed([font({ textStyle: 'caption' })], 44) ?? <Text modifiers={[font({ textStyle: 'caption' })]}> </Text>)
    ),
    minimal: <Image systemName={symbol} color={accent} />,
    expandedLeading: <Image systemName={symbol} color={accent} size={28} />,
    expandedCenter: <Text modifiers={[font({ textStyle: 'headline' }), lineLimit(1)]}>{props.title}</Text>,
    expandedBottom: <Text modifiers={[font({ textStyle: 'subheadline' }), muted, lineLimit(1)]}>{props.detail}</Text>,
    expandedTrailing: elapsed([font({ textStyle: 'caption' }), muted], 56),
  };
};

export const turnActivity = createLiveActivity<TurnActivityProps>(TURN_ACTIVITY, TurnActivityLayout);
