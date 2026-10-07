import { useEffect, useMemo, useRef, useState, type ReactNode } from 'react';
import { Pressable, StyleSheet, Text, TextInput, View } from 'react-native';
import { useKeyboardState } from 'react-native-keyboard-controller';
import { useSafeAreaInsets } from 'react-native-safe-area-context';

import { FONT_SCALE } from '@/constants/typeScale';
import { usePalette } from '@/constants/use-palette';
import { useDesign } from '@/design/DesignProvider';
import { GlassSurface, haptic, hitSlopFor, Icon, ramp, uiPlatform } from '@/ui';
import {
  EVERYONE,
  deriveMentions,
  detectTrigger,
  replaceRange,
  type ActiveTrigger,
  type DerivedMentions,
  type MentionCandidate,
  type Selection,
  type TriggerChar,
} from '@/features/chat/mentions/mentions';
import { SuggestionStrip, type Suggestion, type SuggestionState } from '@/features/chat/SuggestionStrip';
import { DictationButton } from '@/features/speech/DictationButton';
import { dictationProblemText, joinDictation } from '@/features/speech/dictation';

/** What the composer hands over on send. */
export interface ComposerSend {
  text: string;
  /** Derived from the final text; null when the conversation offers no one to mention. */
  mentions: DerivedMentions | null;
}

/** The answer to a pick from an outside source (`#` agent, `~` skill, `/` tool). */
export type PickResult = { insert: string } | { error: string };

export interface ComposerProps {
  /** Why sending is impossible; renders instead of the input. */
  disabledReason?: string | null;
  busy: boolean;
  /** Returns (or resolves to) an error text when the message was not accepted. */
  onSend: (input: ComposerSend) => string | null | Promise<string | null>;
  /** The other people in the conversation ("@"); "Everyone" is offered with them. */
  people?: readonly MentionCandidate[];
  /** A one-line note under the input (e.g. where mentions notify). */
  hint?: string | null;
  /** Triggers served from outside (the screen fetches their suggestions). */
  externalTriggers?: readonly TriggerChar[];
  /** Told whenever the outside trigger under the caret changes (null: none). */
  onTriggerChange?: (trigger: ActiveTrigger | null) => void;
  /** The suggestions for the current outside trigger. */
  externalSuggestions?: SuggestionState | null;
  /** A pick from an outside strip: what to put in place of the token, or why not. */
  onPickExternal?: (trigger: ActiveTrigger, item: Suggestion) => Promise<PickResult> | PickResult;
  /** Rendered above the input row (the target chip, the attachment tray). */
  accessory?: ReactNode;
  /** Rendered left of the input (the attachment button). */
  leading?: ReactNode;
  /** Sending is allowed with empty text (files attached). */
  allowEmptyText?: boolean;
  /**
   * While an answer streams: stops it on the server (the Send button
   * becomes Stop). Resolves to an error text (or null), and the message to
   * put back when the server removed it with the stopped answer. Absent:
   * Send stays, disabled, until the answer ends.
   */
  onStop?: (() => Promise<{ problem: string | null; restoreText: string | null; lostAttachments?: string[]; restoredAttachments?: string[] }>) | null;
  /** Offer on-device dictation (the screen checks the device can). */
  dictation?: boolean;
  /** Text the input starts with (Siri "Ask <agent>"); never sent until the user taps Send. */
  initialText?: string | null;
}

function listNames(names: string[]): string {
  if (names.length <= 1) return names.join('');
  return `${names.slice(0, -1).join(', ')} and ${names[names.length - 1]}`;
}

/** What the composer says after a Stop removed the user's question on the server. */
export function stoppedNote(restoreText: string, lostAttachments: string[], restoredAttachments: string[] = []): string {
  const hasText = restoreText.trim() !== '';
  const noneBack = restoredAttachments.length === 0;
  const subject = hasText
    ? noneBack
      ? 'Your message is'
      : 'Your message and its files are'
    : restoredAttachments.length === 1
      ? 'Its file is'
      : 'Its files are';
  if (lostAttachments.length === 0) return `Stopped before an answer. ${subject} back here to edit or send again.`;
  const files = listNames(lostAttachments);
  const them = lostAttachments.length === 1 ? 'it' : 'them';
  if (!hasText && noneBack) return `Stopped before an answer. The message was removed; attach ${files} again to send ${them}.`;
  return `Stopped before an answer. ${subject} back here; attach ${files} again to send ${them}.`;
}

/** The "@" rows for a query: Everyone first, then names starting with it, then names containing it. */
export function peopleSuggestions(people: readonly MentionCandidate[], query: string): Suggestion[] {
  const q = query.toLocaleLowerCase();
  const rows: Suggestion[] = [];
  if (people.length > 0 && EVERYONE.toLocaleLowerCase().startsWith(q)) {
    rows.push({ key: 'everyone', title: EVERYONE, subtitle: 'Notify everyone in this project', initial: '@' });
  }
  const named = people.filter((p) => p.name.trim() !== '');
  const starts = named.filter((p) => p.name.toLocaleLowerCase().startsWith(q));
  const contains = named.filter((p) => !starts.includes(p) && p.name.toLocaleLowerCase().includes(q));
  for (const p of [...starts, ...contains]) rows.push({ key: `user-${p.userId}`, title: p.name });
  return rows;
}

export function Composer({
  disabledReason,
  busy,
  onSend,
  people = [],
  hint,
  externalTriggers = [],
  onTriggerChange,
  externalSuggestions,
  onPickExternal,
  accessory,
  leading,
  allowEmptyText = false,
  onStop = null,
  dictation = false,
  initialText = null,
}: ComposerProps) {
  const palette = usePalette();
  const design = useDesign();
  // Off the keyboard, the bar keeps clear of the home indicator / Android gesture bar
  // (edge-to-edge, audit 2.11 S1); on the keyboard it sits right on top of it.
  const insets = useSafeAreaInsets();
  const keyboardUp = useKeyboardState((state) => state.isVisible);
  const bottomInset = keyboardUp ? design.space[2] : Math.max(insets.bottom, design.space[2]);
  const [text, setText] = useState(initialText ?? '');
  const [selection, setSelection] = useState<Selection>({ start: 0, end: 0 });
  /** Set once after a pick to move the caret; released on the next selection event. */
  const [forcedSelection, setForcedSelection] = useState<Selection | undefined>(undefined);
  const [error, setError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);
  const [picking, setPicking] = useState(false);
  const [stopping, setStopping] = useState(false);
  /** The text typed before the current dictation; the words heard go after it. */
  const dictationBase = useRef<string | null>(null);
  /** A token whose pick failed; its strip stays hidden until the token changes. */
  const [dismissed, setDismissed] = useState<string | null>(null);

  const triggers = useMemo<TriggerChar[]>(
    () => [...(people.length > 0 ? (['@'] as TriggerChar[]) : []), ...externalTriggers],
    [people.length, externalTriggers],
  );
  const detected = detectTrigger(text, selection, triggers);
  const tokenKey = detected ? `${detected.start}:${detected.trigger}${detected.query}` : null;
  const active = detected && tokenKey !== dismissed ? detected : null;
  const external = active && active.trigger !== '@' ? active : null;
  const externalKey = external ? `${external.start}:${external.trigger}${external.query}` : null;

  useEffect(() => {
    onTriggerChange?.(external);
    // Re-announced only when the token itself changes.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [externalKey]);

  if (disabledReason) {
    return (
      <View style={[styles.bar, { backgroundColor: palette.background, paddingBottom: bottomInset }]}>
        <Text style={[styles.notice, { color: palette.textSecondary }]}>{disabledReason}</Text>
      </View>
    );
  }

  const canSend = !busy && !submitting && !picking && (text.trim() !== '' || allowEmptyText);
  const showStop = busy && onStop !== null;
  const stop = () => {
    if (!onStop || stopping) return;
    setStopping(true);
    setError(null);
    onStop()
      .then(({ problem, restoreText, lostAttachments = [], restoredAttachments = [] }) => {
        if (restoreText !== null) {
          // The server removed the question with the stopped answer: hand it
          // back, unless the user has typed something new meanwhile. Its files
          // are back in the tray when the screen still held them; the rest are named.
          setText((current) => (current.trim() === '' ? restoreText : current));
          setError(stoppedNote(restoreText, lostAttachments, restoredAttachments));
          return;
        }
        setError(problem);
      })
      .catch(() => setError("Couldn't stop the answer. Try again."))
      .finally(() => setStopping(false));
  };

  const settle = (sent: string, problem: string | null) => {
    setError(problem);
    // Only the accepted text is cleared; an edit made meanwhile is kept.
    if (problem === null) setText((current) => (current === sent ? '' : current));
  };
  const submit = () => {
    if (!canSend) return;
    haptic('send');
    const sent = text;
    const mentions = people.length > 0 ? deriveMentions(sent, people, { everyone: true }) : null;
    const result = onSend({ text: sent, mentions });
    if (!(result instanceof Promise)) {
      settle(sent, result);
      return;
    }
    setSubmitting(true);
    result
      .then((problem) => settle(sent, problem))
      .catch(() => settle(sent, 'This message could not be sent.'))
      .finally(() => setSubmitting(false));
  };

  const applyInsert = (trigger: ActiveTrigger, insert: string) => {
    const next = replaceRange(text, trigger.start, trigger.end, insert);
    setDismissed(null);
    setText(next.text);
    setSelection({ start: next.caret, end: next.caret });
    setForcedSelection({ start: next.caret, end: next.caret });
    setError(null);
  };

  const pick = (item: Suggestion) => {
    if (!active) return;
    if (active.trigger === '@') {
      const name = item.key === 'everyone' ? EVERYONE : item.title;
      applyInsert(active, `@${name} `);
      return;
    }
    if (!onPickExternal) return;
    const trigger = active;
    const outcome = onPickExternal(trigger, item);
    const finish = (result: PickResult) => {
      if ('error' in result) {
        setError(result.error);
        setDismissed(tokenKey);
      } else {
        applyInsert(trigger, result.insert);
      }
    };
    if (!(outcome instanceof Promise)) {
      finish(outcome);
      return;
    }
    setPicking(true);
    outcome
      .then(finish)
      .catch(() => finish({ error: 'That could not be added. Try again.' }))
      .finally(() => setPicking(false));
  };

  const stripState: SuggestionState | null = !active
    ? null
    : active.trigger === '@'
      ? { items: peopleSuggestions(people, active.query) }
      : (externalSuggestions ?? { items: [], loading: true });

  return (
    <View style={[styles.bar, { backgroundColor: palette.background, paddingBottom: bottomInset }]}>
      {stripState ? (
        <SuggestionStrip
          state={picking ? { ...stripState, loading: true } : stripState}
          onPick={pick}
          label={active?.trigger === '@' ? 'People' : 'Suggestions'}
        />
      ) : null}
      {accessory}
      {error ? (
        <Text accessibilityRole="alert" style={[styles.error, { color: palette.danger }]}>
          {error}
        </Text>
      ) : null}
      <GlassSurface radius={design.radius.composer} interactive floating style={styles.row}>
        {leading}
        {dictation ? (
          <DictationButton
            disabled={submitting}
            onStart={() => {
              dictationBase.current = text;
              setError(null);
            }}
            onText={(heard, final) => {
              const base = dictationBase.current ?? '';
              const next = joinDictation(base, heard);
              setText(next);
              setSelection({ start: next.length, end: next.length });
              if (final) dictationBase.current = next;
            }}
            onProblem={(problem) => setError(dictationProblemText(problem))}
          />
        ) : null}
        <TextInput
          value={text}
          selection={forcedSelection}
          onChangeText={(value) => {
            // Typing at the end keeps the caret at the end; the platform's
            // selection event (when it comes) is the truth.
            if (selection.start === selection.end && selection.end === text.length) {
              setSelection({ start: value.length, end: value.length });
            }
            setText(value);
            setError(null);
          }}
          onSelectionChange={(event) => {
            setSelection(event.nativeEvent.selection);
            setForcedSelection(undefined);
          }}
          placeholder="Message"
          placeholderTextColor={design.colors.textTertiary}
          multiline
          maxFontSizeMultiplier={FONT_SCALE.input}
          accessibilityLabel="Message"
          style={[styles.input, { color: palette.text }]}
        />
        {showStop ? (
          <Pressable
            accessibilityRole="button"
            accessibilityLabel="Stop answer"
            accessibilityState={{ disabled: stopping, busy: stopping }}
            disabled={stopping}
            onPress={stop}
            hitSlop={hitSlopFor(SEND_SIZE)}
            android_ripple={{ color: design.colors.surface, borderless: true }}
            style={({ pressed }) => [
              styles.send,
              { backgroundColor: design.colors.textPrimary, opacity: stopping ? 0.38 : pressed && uiPlatform === 'ios' ? 0.7 : 1 },
            ]}>
            <Icon name="stop" size={16} color={design.colors.background} />
          </Pressable>
        ) : (
          <Pressable
            accessibilityRole="button"
            accessibilityLabel="Send"
            accessibilityState={{ disabled: !canSend, busy }}
            disabled={!canSend}
            onPress={submit}
            hitSlop={hitSlopFor(SEND_SIZE)}
            android_ripple={{ color: design.colors.accentPressed, borderless: true }}
            style={({ pressed }) => [
              styles.send,
              { backgroundColor: pressed && uiPlatform === 'ios' ? design.colors.accentPressed : design.colors.accent, opacity: canSend ? 1 : 0.38 },
            ]}>
            <Icon name="send" size={18} color={design.colors.accentOn} weight="bold" />
          </Pressable>
        )}
      </GlassSurface>
      {hint ? <Text style={[styles.hint, { color: palette.textSecondary }]}>{hint}</Text> : null}
    </View>
  );
}

/** The send / stop circle (§4.5): 36 pt visual, grown to the 44/48 touch target. */
const SEND_SIZE = 36;

const styles = StyleSheet.create({
  bar: { paddingHorizontal: 12, paddingTop: 8, gap: 4 },
  row: { flexDirection: 'row', alignItems: 'flex-end', gap: 4, minHeight: 52, paddingHorizontal: 8, paddingVertical: 8 },
  input: {
    ...ramp.body,
    flex: 1,
    minHeight: 36,
    maxHeight: 140,
    paddingHorizontal: 8,
    paddingTop: 7,
    paddingBottom: 7,
  },
  send: { width: SEND_SIZE, height: SEND_SIZE, borderRadius: SEND_SIZE / 2, alignItems: 'center', justifyContent: 'center' },
  notice: { ...ramp.footnote, textAlign: 'center', padding: 8 },
  error: { ...ramp.footnote },
  hint: { ...ramp.caption },
});
