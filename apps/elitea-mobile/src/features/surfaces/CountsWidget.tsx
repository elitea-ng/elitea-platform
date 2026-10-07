import { HStack, Image, Spacer, Text, VStack } from '@expo/ui/swift-ui';
import {
  containerBackground,
  dynamicTypeSize,
  font,
  foregroundStyle,
  lineLimit,
  minimumScaleFactor,
  monospacedDigit,
  widgetURL,
} from '@expo/ui/swift-ui/modifiers';
import { createWidget, type WidgetEnvironment } from 'expo-widgets';

import { signedOutSnapshot, type SurfaceSnapshot } from '@/features/surfaces/surfaceSnapshot';

/**
 * The home-screen and lock-screen widget (`ZefirCounts` in app.config.ts):
 * pending approvals, unread notifications and running answers of the active
 * workspace, from the App Group snapshot (src/features/surfaces/surfaceSnapshot.ts).
 *
 * The layout runs in the widget extension's own JS runtime: it may use only
 * `@expo/ui/swift-ui` views and modifiers and its props (the 'widget'
 * directive serialises the function; nothing from this module's scope is
 * visible to it). Titles and the host appear only when the snapshot carries
 * them (the policy allows titles); otherwise the widget shows counts.
 */
export const COUNTS_WIDGET = 'ZefirCounts';

const CountsLayout = (props: SurfaceSnapshot, environment: WidgetEnvironment) => {
  'widget';
  const p = props ?? { signedIn: false, approvals: 0, unread: 0, running: 0, host: '', titles: [], openUrl: '' };
  const family = environment.widgetFamily ?? 'systemSmall';
  const dark = environment.colorScheme === 'dark';
  // In full colour the widget wears the app's pastel ground and Zefir rose (design-tokens.json:
  // accent light #B4306A / dark #FF8CB8, gradient stops of backgroundGradient). On a tinted or
  // clear home screen (accented / vibrant) the system recolours it, so the ground is the system's.
  const fullColor = (environment.widgetRenderingMode ?? 'fullColor') === 'fullColor';
  const accent = dark ? '#FF8CB8' : '#B4306A';
  const short = (n: number) => (n > 99 ? '99+' : String(n));
  const ground = fullColor
    ? containerBackground(
        {
          type: 'linearGradient',
          colors: dark ? ['#1F1520', '#16151F', '#111B19'] : ['#FCEBF2', '#F2EEFB', '#E8F5EF'],
          startPoint: { x: 0.3, y: 0 },
          endPoint: { x: 0.7, y: 1 },
        },
        'widget',
      )
    : containerBackground({ type: 'hierarchical', style: 'quinary' }, 'widget');
  // The lock-screen families always take the system's material.
  const lockGround = containerBackground({ type: 'hierarchical', style: 'quinary' }, 'widget');
  const muted = foregroundStyle({ type: 'hierarchical', style: 'secondary' });
  const tint = fullColor ? [foregroundStyle(accent)] : [];

  if (!p.signedIn) {
    if (family === 'accessoryInline') return <Text modifiers={[widgetURL(p.openUrl)]}>Agent Zefir</Text>;
    if (family === 'accessoryCircular') {
      return <Image systemName="lock.fill" modifiers={[widgetURL(p.openUrl), lockGround]} />;
    }
    return (
      <VStack alignment="leading" spacing={4} modifiers={[widgetURL(p.openUrl), family === 'accessoryRectangular' ? lockGround : ground]}>
        <Text modifiers={[font({ textStyle: 'headline', design: 'rounded' })]}>Agent Zefir</Text>
        <Text modifiers={[font({ textStyle: 'caption' }), muted]}>Open the app to sign in.</Text>
      </VStack>
    );
  }

  const approvals = p.approvals;
  const total = approvals + p.unread;
  // Where a tap goes: a decision waiting wins, then the inbox, else the app.
  const tapUrl = approvals > 0 ? p.approvalsUrl : p.unread > 0 ? p.notificationsUrl : p.openUrl;

  if (family === 'accessoryInline') {
    const parts = [] as string[];
    if (approvals > 0) parts.push(`${short(approvals)} waiting`);
    if (p.unread > 0) parts.push(`${short(p.unread)} unread`);
    if (p.running > 0) parts.push(`${short(p.running)} running`);
    return <Text modifiers={[widgetURL(tapUrl)]}>{parts.length > 0 ? parts.join(' · ') : 'All caught up'}</Text>;
  }

  if (family === 'accessoryCircular') {
    return (
      <VStack spacing={0} modifiers={[widgetURL(tapUrl), lockGround]}>
        <Image systemName={approvals > 0 ? 'hand.raised.fill' : 'bell.fill'} />
        <Text modifiers={[font({ textStyle: 'title3', weight: 'bold', design: 'rounded' })]}>{short(total)}</Text>
      </VStack>
    );
  }

  // Text styles, never fixed sizes: the widget follows Dynamic Type like the app does.
  const row = (symbol: 'hand.raised.fill' | 'bell.fill' | 'ellipsis.bubble.fill', value: number, label: string) => (
    <HStack spacing={6}>
      <Image systemName={symbol} modifiers={[...tint, font({ textStyle: 'caption' })]} />
      <Text modifiers={[font({ textStyle: 'subheadline', weight: 'semibold', design: 'rounded' }), monospacedDigit()]}>{short(value)}</Text>
      <Text modifiers={[font({ textStyle: 'caption' }), muted, lineLimit(1)]}>{label}</Text>
    </HStack>
  );

  if (family === 'accessoryRectangular') {
    return (
      <VStack alignment="leading" spacing={1} modifiers={[widgetURL(tapUrl), lockGround]}>
        {row('hand.raised.fill', approvals, 'waiting for you')}
        {row('bell.fill', p.unread, 'unread')}
        {row('ellipsis.bubble.fill', p.running, 'running')}
      </VStack>
    );
  }

  // Home screen: the workspace (or the app) on top, the decisions waiting as the hero number,
  // unread and running under it; the medium widget lists the waiting conversations when the
  // workspace allows titles.
  const header = p.host || 'Agent Zefir';
  const hero = (
    <VStack alignment="leading" spacing={0}>
      <Text
        modifiers={[
          font({ textStyle: 'largeTitle', weight: 'bold', design: 'rounded' }),
          monospacedDigit(),
          lineLimit(1),
          minimumScaleFactor(0.6),
          ...(approvals > 0 ? tint : []),
        ]}>
        {short(approvals)}
      </Text>
      <Text modifiers={[font({ textStyle: 'caption', weight: 'medium' }), muted, lineLimit(1)]}>waiting for you</Text>
    </VStack>
  );
  // A home-screen widget is about 158 pt tall whatever the text size (F6): the text follows Dynamic
  // Type up to xLarge, and the small widget puts unread and running on one line, so the counts
  // under the hero number are never cut off at the larger sizes WidgetKit honours.
  const small = family === 'systemSmall';
  return (
    <VStack alignment="leading" spacing={6} modifiers={[widgetURL(tapUrl), ground, dynamicTypeSize({ max: 'xLarge' })]}>
      <HStack spacing={4}>
        <Image systemName="sparkles" modifiers={[...tint, font({ textStyle: 'caption2', weight: 'semibold' })]} />
        <Text modifiers={[font({ textStyle: 'caption', weight: 'semibold' }), ...tint, lineLimit(1)]}>{header}</Text>
      </HStack>
      {hero}
      <Spacer />
      {small ? (
        <HStack spacing={10} modifiers={[lineLimit(1), minimumScaleFactor(0.8)]}>
          {row('bell.fill', p.unread, 'unread')}
          {row('ellipsis.bubble.fill', p.running, 'running')}
        </HStack>
      ) : (
        <VStack alignment="leading" spacing={6}>
          {row('bell.fill', p.unread, 'unread')}
          {row('ellipsis.bubble.fill', p.running, 'running')}
        </VStack>
      )}
      {family !== 'systemSmall' && p.titles.length > 0 ? (
        <VStack alignment="leading" spacing={2}>
          {p.titles.map((title) => (
            <Text key={title} modifiers={[font({ textStyle: 'caption2' }), muted, lineLimit(1)]}>
              {title}
            </Text>
          ))}
        </VStack>
      ) : null}
    </VStack>
  );
};

export const countsWidget = createWidget<SurfaceSnapshot>(COUNTS_WIDGET, CountsLayout, signedOutSnapshot(0));
