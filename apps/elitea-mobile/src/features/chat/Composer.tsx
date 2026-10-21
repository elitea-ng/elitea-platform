import { useEffect, useMemo, useRef, useState, type ComponentRef, type ReactNode } from 'react';
import { AccessibilityInfo, Pressable, StyleSheet, TextInput, View } from 'react-native';
import { useKeyboardState } from 'react-native-keyboard-controller';
import { useSafeAreaInsets } from 'react-native-safe-area-context';

import { FONT_SCALE } from '@/constants/typeScale';
import { useDesign } from '@/design/DesignProvider';
import { GlassSurface, haptic, hitSlopFor, Icon, IconButton, ramp, Text, uiPlatform } from '@/ui';
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
import { holdsOnlySent } from '@/features/chat/keyboardCommit';
import { DictationPanel } from '@/features/speech/DictationPanel';
import { dictationProblemText, joinDictation, useDictation } from '@/features/speech/dictation';

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
  /** Start dictating once, on open (Home's "Dictate a message"). */
  autoDictate?: boolean;
  /** Text the input starts with (Siri "Ask <agent>"); never sent until the user taps Send. */
  initialText?: string | null;
  /** The input's placeholder ("Message Research…"). */
  placeholder?: string;
  /** Text put in the input from outside (an empty-state suggestion); a new `key` puts it again. */
  draft?: { text: string; key: number } | null;
  /**
   * Development previews only (`/design-chat`): draw the composer's own listening state with
   * these words, as if dictation had heard them, without starting the recogniser.
   */
  dictationPreview?: { heard: string; level: number } | null;
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
  autoDictate = false,
  initialText = null,
  placeholder = 'Message',
  draft = null,
  dictationPreview = null,
}: ComposerProps) {
  const design = useDesign();
  const { colors } = design;
  // Off the keyboard, the bar keeps clear of the home indicator / Android gesture bar
  // (edge-to-edge, audit 2.11 S1); on the keyboard it sits right on top of it.
  const insets = useSafeAreaInsets();
  const keyboardUp = useKeyboardState((state) => state.isVisible);
  const bottomInset = keyboardUp ? design.space[2] : Math.max(insets.bottom, design.space[2]);
  const input = useRef<ComponentRef<typeof TextInput>>(null);
  const [text, setText] = useState(dictationPreview ? joinDictation(initialText ?? '', dictationPreview.heard) : (initialText ?? ''));
  const [selection, setSelection] = useState<Selection>({ start: 0, end: 0 });
  /** Set once after a pick to move the caret; released on the next selection event. */
  const [forcedSelection, setForcedSelection] = useState<Selection | undefined>(undefined);
  const [error, setError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);
  const [picking, setPicking] = useState(false);
  const [stopping, setStopping] = useState(false);
  /** When the last send read the text, and when the text last changed after it (#56). */
  const sentAt = useRef(0);
  const changedAt = useRef<number | null>(null);
  /** The text typed before the current dictation; the words heard go after it. */
  const dictationBase = useRef<string | null>(null);
  /** What dictation has heard so far (the panel shows it) and the microphone level. */
  const [heard, setHeard] = useState('');
  const [level, setLevel] = useState(0);
  const voice = useDictation({
    onText: (words, final) => {
      const base = dictationBase.current ?? '';
      const next = joinDictation(base, words);
      setHeard(words.trim());
      setText(next);
      setSelection({ start: next.length, end: next.length });
      if (final) dictationBase.current = next;
    },
    onProblem: (problem) => setError(dictationProblemText(problem)),
    onLevel: setLevel,
  });
  const startDictation = () => {
    dictationBase.current = text;
    setHeard('');
    setLevel(0);
    setError(null);
    haptic('voiceStart');
    void voice.start();
  };
  // Home's "Dictate a message": once, on open (a tap the user made there).
  const autoStarted = useRef(false);
  useEffect(() => {
    if (!autoDictate || !dictation || autoStarted.current || disabledReason) return;
    autoStarted.current = true;
    startDictation();
  });
  // An empty-state suggestion goes into the input to edit or send; never sent on its own. Taken
  // over while rendering (a new key), then the input is focused.
  const [draftKey, setDraftKey] = useState<number | null>(null);
  if (draft !== null && draft.key !== draftKey) {
    setDraftKey(draft.key);
    setText(draft.text);
    setSelection({ start: draft.text.length, end: draft.text.length });
    setError(null);
  }
  useEffect(() => {
    if (draftKey !== null) input.current?.focus();
  }, [draftKey]);
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

  // Starting dictation removes the Dictate button and ending it removes the panel, so the
  // screen-reader focus would fall to an arbitrary element (and iOS ignores the panel's live
  // region). Focus follows the control instead: to Stop when listening starts ("Listening" is
  // announced), back to Dictate when it ends.
  const micButton = useRef<ComponentRef<typeof View>>(null);
  const stopButton = useRef<ComponentRef<typeof View>>(null);
  const wasListening = useRef(false);
  useEffect(() => {
    if (voice.listening === wasListening.current) return;
    wasListening.current = voice.listening;
    if (voice.listening) AccessibilityInfo.announceForAccessibility('Listening');
    const timer = setTimeout(() => {
      const target = voice.listening ? stopButton.current : (micButton.current ?? input.current);
      if (target) AccessibilityInfo.sendAccessibilityEvent(target, 'focus');
    }, FOCUS_DELAY_MS);
    return () => clearTimeout(timer);
  }, [voice.listening]);

  // The composer can turn into a notice mid-dictation (an approval pauses the answer): the
  // panel with Stop and Cancel goes, so the recogniser must too, or the microphone stays live
  // with no control and keeps writing into a hidden input. What was heard so far is kept.
  const recognising = voice.listening;
  useEffect(() => {
    if (!disabledReason || !recognising) return;
    voice.cancel();
    dictationBase.current = null;
    // `voice` is a new object every render; only these two decide.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [disabledReason, recognising]);
  /** The listening state as drawn: the recogniser's, or a development preview's. */
  const listening = voice.listening || dictationPreview !== null;

  if (disabledReason) {
    return (
      <View style={[styles.bar, { paddingHorizontal: design.space.composerInset, paddingBottom: bottomInset }]}>
        <GlassSurface radius={design.radius.composer} floating style={styles.notice}>
          <Icon name="info" size={18} tone="secondary" />
          <Text variant="footnote" tone="secondary" style={styles.noticeText}>
            {disabledReason}
          </Text>
        </GlassSurface>
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
    if (problem !== null) return;
    // Only the accepted text is cleared; an edit made meanwhile is kept. A pending keyboard
    // autocorrection that iOS committed right after Send is not an edit (#56).
    const at = sentAt.current;
    const last = changedAt.current;
    setText((current) => (holdsOnlySent(sent, current, at, last) ? '' : current));
  };
  const submit = () => {
    if (!canSend) return;
    haptic('send');
    // Abort, not stop: a stop makes the recogniser deliver a final result, which would write
    // the sent words back into the cleared input (and a second tap sends them twice, #56).
    // What is sent is what the input shows now.
    if (voice.listening) voice.cancel();
    dictationBase.current = null;
    const sent = text;
    sentAt.current = Date.now();
    changedAt.current = null;
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
    <View style={[styles.bar, { paddingHorizontal: design.space.composerInset, paddingBottom: bottomInset }]}>
      {listening ? (
        <DictationPanel
          stopRef={stopButton}
          heard={dictationPreview?.heard ?? heard}
          level={dictationPreview?.level ?? level}
          onStop={() => voice.stop()}
          onKeyboard={() => {
            voice.stop();
            input.current?.focus();
          }}
          onCancel={() => {
            const base = dictationBase.current ?? '';
            voice.cancel();
            setText(base);
            setSelection({ start: base.length, end: base.length });
          }}
        />
      ) : null}
      {stripState ? (
        <SuggestionStrip
          state={picking ? { ...stripState, loading: true } : stripState}
          onPick={pick}
          label={active?.trigger === '@' ? 'People' : 'Suggestions'}
        />
      ) : null}
      {error ? (
        <GlassSurface radius={design.radius.md} style={styles.error}>
          <Icon name="error" size={18} tone="danger" />
          <Text variant="footnote" tone="danger" accessibilityRole="alert" style={styles.noticeText}>
            {error}
          </Text>
        </GlassSurface>
      ) : null}
      <GlassSurface radius={design.radius.composer} interactive floating style={styles.box}>
        {accessory ? <View style={styles.accessory}>{accessory}</View> : null}
        <View style={styles.row}>
          {leading}
          <TextInput
            ref={input}
            value={text}
            selection={forcedSelection}
            onChangeText={(value) => {
              // Typing at the end keeps the caret at the end; the platform's
              // selection event (when it comes) is the truth.
              if (selection.start === selection.end && selection.end === text.length) {
                setSelection({ start: value.length, end: value.length });
              }
              changedAt.current = Date.now();
              setText(value);
              setError(null);
            }}
            onSelectionChange={(event) => {
              setSelection(event.nativeEvent.selection);
              setForcedSelection(undefined);
            }}
            placeholder={placeholder}
            placeholderTextColor={colors.textTertiary}
            multiline
            maxFontSizeMultiplier={FONT_SCALE.input}
            accessibilityLabel="Message"
            accessibilityHint={placeholder !== 'Message' ? placeholder : undefined}
            style={[styles.input, { color: colors.textPrimary }]}
          />
          {dictation && !listening ? (
            <IconButton
              ref={micButton}
              icon="mic"
              variant="plain"
              size={SEND_SIZE}
              accessibilityLabel="Dictate"
              accessibilityHint="Speech is turned into text on this device"
              disabled={submitting}
              onPress={startDictation}
            />
          ) : null}
          {showStop ? (
            <Pressable
              accessibilityRole="button"
              accessibilityLabel="Stop answer"
              accessibilityState={{ disabled: stopping, busy: stopping }}
              disabled={stopping}
              onPress={stop}
              hitSlop={hitSlopFor(SEND_SIZE)}
              android_ripple={{ color: colors.surface, borderless: true }}
              style={({ pressed }) => [
                styles.send,
                { backgroundColor: colors.textPrimary, opacity: stopping ? 0.38 : pressed && uiPlatform === 'ios' ? 0.7 : 1 },
              ]}>
              <Icon name="stop" size={14} color={colors.background} scales={false} />
            </Pressable>
          ) : (
            <Pressable
              accessibilityRole="button"
              accessibilityLabel="Send"
              accessibilityState={{ disabled: !canSend, busy }}
              disabled={!canSend}
              onPress={submit}
              hitSlop={hitSlopFor(SEND_SIZE)}
              android_ripple={{ color: colors.accentPressed, borderless: true }}
              style={({ pressed }) => [
                styles.send,
                canSend
                  ? { backgroundColor: pressed && uiPlatform === 'ios' ? colors.accentPressed : colors.accent }
                  : { backgroundColor: colors.surfaceSunken, borderWidth: StyleSheet.hairlineWidth, borderColor: colors.separator },
              ]}>
              <Icon name="send" size={18} color={canSend ? colors.accentOn : colors.textTertiary} weight="bold" scales={false} />
            </Pressable>
          )}
        </View>
      </GlassSurface>
      {hint ? (
        <Text variant="caption" tone="secondary" style={styles.hint}>
          {hint}
        </Text>
      ) : null}
    </View>
  );
}

/** After a dictation control appears, before screen-reader focus moves to it (lets it mount). */
const FOCUS_DELAY_MS = 150;

/** The send / stop circle (§4.5): 36 pt visual, grown to the 44/48 touch target. */
const SEND_SIZE = 36;

const styles = StyleSheet.create({
  bar: { paddingTop: 8, gap: 8 },
  box: { paddingHorizontal: 6, paddingVertical: 6, gap: 6 },
  accessory: { gap: 6, paddingHorizontal: 4, paddingTop: 2 },
  row: { flexDirection: 'row', alignItems: 'flex-end', gap: 2, minHeight: 40 },
  input: {
    ...ramp.body,
    flex: 1,
    minHeight: 36,
    // About six lines of body text, then it scrolls (§4.5).
    maxHeight: 140,
    paddingHorizontal: 8,
    paddingTop: 8,
    paddingBottom: 8,
  },
  send: { width: SEND_SIZE, height: SEND_SIZE, borderRadius: SEND_SIZE / 2, alignItems: 'center', justifyContent: 'center', marginStart: 2 },
  notice: { flexDirection: 'row', alignItems: 'center', gap: 8, minHeight: 52, paddingHorizontal: 16, paddingVertical: 10 },
  noticeText: { flexShrink: 1 },
  error: { flexDirection: 'row', alignItems: 'center', gap: 8, paddingHorizontal: 12, paddingVertical: 8 },
  hint: { paddingHorizontal: 12 },
});
