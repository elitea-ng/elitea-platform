import { StyleSheet, View } from 'react-native';

import { useDesign } from '@/design/DesignProvider';
import { Button, GlassSurface, Icon, Text } from '@/ui';

/**
 * What a Local work thread shows above the composer while the cloud copy runs it (ADR-0034), as
 * the web's Cloud executor notice: where the thread runs, and the two things a person can do
 * from here.
 *
 * - "Ask laptop to take back": the owner only; it notifies the owner's laptop and changes
 *   nothing in the thread (only the laptop can move it), so the composer keeps working;
 * - "Erase cloud copy": the owner or a project settings editor; it asks first.
 * - "Pushes and pull requests: <credential>" (client contract 1.29): the owner only, while the
 *   cloud runs it; it opens the sheet that chooses the GitHub or GitLab configuration the cloud
 *   agent pushes and opens pull requests through.
 *
 * `returning` (the laptop is taking the thread back) replaces the composer elsewhere; here it
 * only keeps Erase within reach.
 */
export function CloudExecutorNotice({
  executor,
  canTakeBack,
  canErase,
  takeBackRequested,
  requesting,
  erasing,
  onTakeBack,
  onErase,
  credentialTitle = null,
  onChooseCredential,
}: {
  executor: 'cloud' | 'returning';
  canTakeBack: boolean;
  canErase: boolean;
  /** The laptop was already asked (the server's `reclaim_pending`, or this session's request). */
  takeBackRequested: boolean;
  requesting: boolean;
  erasing: boolean;
  onTakeBack: () => void;
  onErase: () => void;
  /** The credential control's words; null hides it (not the owner, or not running in the cloud). */
  credentialTitle?: string | null;
  onChooseCredential?: () => void;
}) {
  const design = useDesign();
  const returning = executor === 'returning';
  const title = returning ? 'Returning to your laptop' : 'Running in the cloud';
  const body = returning
    ? 'You can read it here; continue it on your laptop once it lands.'
    : takeBackRequested
      ? 'Your laptop was asked to take it back. It starts when the running cloud turn ends.'
      : 'This thread works on a copy of your laptop folder in the cloud.';
  const showTakeBack = canTakeBack && !returning;
  const showCredential = credentialTitle !== null && !returning && onChooseCredential !== undefined;
  return (
    <GlassSurface radius={design.radius.md} floating style={styles.card} testID="cloud-executor-notice" accessible={false}>
      <View style={styles.head}>
        <Icon name="info" size={18} tone="secondary" />
        <View style={styles.flex}>
          <Text variant="subheadline" emphasized accessibilityRole="header" testID="cloud-executor-badge">
            {title}
          </Text>
          <Text variant="footnote" tone="secondary">
            {body}
          </Text>
        </View>
      </View>
      {showCredential ? (
        <Button title={credentialTitle} size="small" variant="plain" onPress={onChooseCredential} testID="cloud-pr-credential" />
      ) : null}
      {showTakeBack || canErase ? (
        <View style={styles.actions}>
          {showTakeBack ? (
            <Button
              title={takeBackRequested ? 'Asked laptop' : 'Ask laptop to take back'}
              size="small"
              variant="tonal"
              loading={requesting}
              disabled={requesting || takeBackRequested}
              onPress={onTakeBack}
              testID="cloud-take-back"
            />
          ) : null}
          {canErase ? (
            <Button title="Erase cloud copy" size="small" variant="destructivePlain" loading={erasing} disabled={erasing} onPress={onErase} testID="cloud-erase" />
          ) : null}
        </View>
      ) : null}
    </GlassSurface>
  );
}

const styles = StyleSheet.create({
  flex: { flex: 1 },
  card: { alignSelf: 'stretch', paddingHorizontal: 14, paddingVertical: 10, gap: 8 },
  head: { flexDirection: 'row', alignItems: 'flex-start', gap: 8 },
  actions: { flexDirection: 'row', flexWrap: 'wrap', gap: 8 },
});
