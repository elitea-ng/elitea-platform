import * as Clipboard from 'expo-clipboard';
import { useState } from 'react';
import { ActivityIndicator, ScrollView, StyleSheet, View } from 'react-native';
import { useSafeAreaInsets } from 'react-native-safe-area-context';

import { useMessageTraceDetail, type PersistedToolStep } from '@/api/queries/messageTraces';
import { useDesign } from '@/design/DesignProvider';
import { useModalAnimation } from '@/features/a11y/motion';
import { toolLabel } from '@/features/chat/ToolCalls';
import { prettyJson, type ToolStep } from '@/features/chat/toolActivity';
import { useShareOut } from '@/security/policy';
import { LockAwareModal } from '@/security/lock/LockAwareModal';
import { Button, haptic, Icon, SegmentedControl, Text } from '@/ui';

type Tab = 'input' | 'output';

function isPersisted(step: ToolStep): step is PersistedToolStep {
  return typeof (step as Partial<PersistedToolStep>).traceId === 'number';
}

/**
 * One tool call's inputs and output, in a sheet: Input/Output tabs,
 * selectable monospace text (JSON pretty-printed), Copy (both only when the
 * workspace allows sharing out), and a banner when
 * the text was cut to the in-memory cap or the output is incomplete. A
 * persisted call's I/O is fetched only now (getMessageTrace).
 */
export function ToolDetailSheet({ step, projectId, onClose }: { step: ToolStep | null; projectId: number | null; onClose: () => void }) {
  const animation = useModalAnimation('slide');
  return (
    <LockAwareModal visible={step !== null} animationType={animation} presentationStyle="pageSheet" onRequestClose={onClose}>
      {step ? <SheetBody step={step} projectId={projectId} onClose={onClose} /> : null}
    </LockAwareModal>
  );
}

function SheetBody({ step, projectId, onClose }: { step: ToolStep; projectId: number | null; onClose: () => void }) {
  const design = useDesign();
  const { colors, radius } = design;
  const insets = useSafeAreaInsets();
  const [tab, setTab] = useState<Tab>(step.status === 'error' ? 'output' : 'input');
  const [copied, setCopied] = useState(false);
  const detail = useMessageTraceDetail(projectId, isPersisted(step) ? step : null);
  const remote = detail.data ?? null;

  const inputs = isPersisted(step) ? (remote?.inputs ?? null) : step.inputs;
  const inputsCut = isPersisted(step) ? (remote?.inputsTruncated ?? false) : step.inputsTruncated;
  const outputText = step.status === 'error' ? step.error : isPersisted(step) ? (remote?.output ?? null) : step.output;
  const outputCut = isPersisted(step) ? (remote?.outputTruncated ?? false) : step.outputTruncated;

  const text = tab === 'input' ? inputs : outputText;
  const shown = text === null ? null : prettyJson(text);
  const banners = [
    (tab === 'input' ? inputsCut : outputCut) ? 'Only the first 8 KB is shown.' : null,
    tab === 'output' && step.partial ? 'Part of this output did not arrive.' : null,
    tab === 'output' && step.status === 'running' ? 'The tool is still running.' : null,
    tab === 'output' && step.status === 'needs_auth' ? 'The tool has not run: its toolkit needs a sign-in.' : null,
    tab === 'output' && step.status === 'waiting'
      ? step.waitingFor === 'answer'
        ? 'The tool has not run: it waits for your answer.'
        : 'The tool has not run: it waits for your approval.'
      : null,
  ].filter((b): b is string => b !== null);
  const loading = isPersisted(step) && detail.isLoading;
  // allow_share_out off: no Copy, and no system selection menu either.
  const shareOut = useShareOut();
  const empty = loading
    ? null
    : detail.error
      ? "Couldn't load this tool call."
      : remote === null && isPersisted(step)
        ? 'Not available on this workspace.'
        : 'Nothing to show.';

  return (
    <View style={[styles.sheet, { backgroundColor: colors.elevated, paddingTop: Math.max(insets.top, 12), paddingBottom: Math.max(insets.bottom, 12) }]}>
      <View style={styles.header}>
        <View style={styles.flex}>
          <Text variant="title3" heading numberOfLines={design.accessibilityLayout ? undefined : 2}>
            {toolLabel(step)}
          </Text>
          {step.parentAgent ? (
            <Text variant="footnote" tone="secondary">
              Run by {step.parentAgent}
            </Text>
          ) : null}
        </View>
        <Button title="Done" variant="plain" size="small" onPress={onClose} />
      </View>
      <SegmentedControl
        accessibilityLabel="Show"
        value={tab}
        onChange={(next) => {
          setTab(next);
          setCopied(false);
        }}
        segments={[
          { value: 'input', label: 'Input' },
          { value: 'output', label: 'Output' },
        ]}
      />
      {banners.map((banner) => (
        <View key={banner} style={[styles.banner, { backgroundColor: colors.warningSoft, borderRadius: radius.md }]}>
          <Icon name="info" size={16} tone="warning" />
          <Text variant="footnote" tone="warning" accessibilityLiveRegion="polite" style={styles.flex}>
            {banner}
          </Text>
        </View>
      ))}
      {loading ? <ActivityIndicator style={styles.loading} color={colors.accent} /> : null}
      <ScrollView style={[styles.body, { backgroundColor: colors.surfaceSunken, borderRadius: radius.md }]} contentContainerStyle={styles.bodyContent}>
        {shown !== null && shown !== '' ? (
          <Text variant="code" selectable={shareOut}>
            {shown}
          </Text>
        ) : empty ? (
          <Text variant="callout" tone="secondary">
            {empty}
          </Text>
        ) : null}
      </ScrollView>
      {shown && shareOut ? (
        <Button
          title={copied ? 'Copied' : 'Copy'}
          icon={copied ? 'check' : 'copy'}
          variant="tonal"
          accessibilityLabel={tab === 'input' ? 'Copy input' : 'Copy output'}
          onPress={() => {
            void Clipboard.setStringAsync(shown).then(() => {
              haptic('copy');
              setCopied(true);
            });
          }}
        />
      ) : null}
    </View>
  );
}

const styles = StyleSheet.create({
  sheet: { flex: 1, paddingHorizontal: 16, gap: 12 },
  header: { flexDirection: 'row', alignItems: 'flex-start', gap: 8 },
  flex: { flex: 1 },
  banner: { flexDirection: 'row', alignItems: 'center', gap: 8, padding: 10, borderCurve: 'continuous' },
  loading: { padding: 8 },
  body: { flex: 1, borderCurve: 'continuous' },
  bodyContent: { padding: 12 },
});
