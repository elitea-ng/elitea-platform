import { HStack, Image, Spacer, Text, VStack } from '@expo/ui/swift-ui';
import { containerBackground, font, foregroundStyle, lineLimit, padding, widgetURL } from '@expo/ui/swift-ui/modifiers';
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
  const accent = '#B4306A';
  const short = (n: number) => (n > 99 ? '99+' : String(n));
  const background = containerBackground({ type: 'hierarchical', style: 'quinary' }, 'widget');
  const muted = foregroundStyle({ type: 'hierarchical', style: 'secondary' });

  if (!p.signedIn) {
    if (family === 'accessoryInline') return <Text modifiers={[widgetURL(p.openUrl)]}>Agent Zefir</Text>;
    if (family === 'accessoryCircular') {
      return <Image systemName="lock.fill" modifiers={[widgetURL(p.openUrl), background]} />;
    }
    return (
      <VStack alignment="leading" spacing={4} modifiers={[widgetURL(p.openUrl), background]}>
        <Text modifiers={[font({ textStyle: 'headline' })]}>Agent Zefir</Text>
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
      <VStack spacing={0} modifiers={[widgetURL(tapUrl), background]}>
        <Image systemName={approvals > 0 ? 'hand.raised.fill' : 'bell.fill'} />
        <Text modifiers={[font({ size: 18, weight: 'bold', design: 'rounded' })]}>{short(total)}</Text>
      </VStack>
    );
  }

  const row = (symbol: 'hand.raised.fill' | 'bell.fill' | 'ellipsis.bubble.fill', value: number, label: string) => (
    <HStack spacing={6}>
      <Image systemName={symbol} color={accent} size={13} />
      <Text modifiers={[font({ size: 15, weight: 'semibold', design: 'rounded' })]}>{short(value)}</Text>
      <Text modifiers={[font({ textStyle: 'caption' }), muted, lineLimit(1)]}>{label}</Text>
    </HStack>
  );

  if (family === 'accessoryRectangular') {
    return (
      <VStack alignment="leading" spacing={1} modifiers={[widgetURL(tapUrl), background]}>
        {row('hand.raised.fill', approvals, 'waiting for you')}
        {row('bell.fill', p.unread, 'unread')}
        {row('ellipsis.bubble.fill', p.running, 'running')}
      </VStack>
    );
  }

  const header = p.host || 'Agent Zefir';
  return (
    <VStack alignment="leading" spacing={6} modifiers={[widgetURL(tapUrl), background, padding({ all: 2 })]}>
      <Text modifiers={[font({ textStyle: 'caption', weight: 'semibold' }), foregroundStyle(accent), lineLimit(1)]}>{header}</Text>
      {row('hand.raised.fill', approvals, 'waiting for you')}
      {row('bell.fill', p.unread, 'unread')}
      {row('ellipsis.bubble.fill', p.running, 'running')}
      {family !== 'systemSmall' && p.titles.length > 0 ? (
        <VStack alignment="leading" spacing={2}>
          {p.titles.map((title) => (
            <Text key={title} modifiers={[font({ textStyle: 'caption2' }), muted, lineLimit(1)]}>
              {title}
            </Text>
          ))}
        </VStack>
      ) : null}
      <Spacer />
    </VStack>
  );
};

export const countsWidget = createWidget<SurfaceSnapshot>(COUNTS_WIDGET, CountsLayout, signedOutSnapshot(0));
