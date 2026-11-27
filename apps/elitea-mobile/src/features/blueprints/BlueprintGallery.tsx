import { useState } from 'react';
import { StyleSheet, View } from 'react-native';

import { Avatar, Button, Card, SegmentedControl, Text } from '@/ui';

import { BLUEPRINT_DATA } from './blueprints.gen';
import type { Blueprint, BlueprintData } from './types';

/**
 * The blueprint gallery (issue 3137): the ready-made agents and teams that
 * show what the platform can do. The app browses them and crafts from them:
 * Craft from it hands the blueprint's brief to the agent builder, which
 * generates a draft for the person to review. Importing a blueprint file is
 * done on the web and the desktop for now.
 *
 * The data is `blueprints.gen.ts`, which `scripts/blueprints.mjs` writes
 * from the repository's `blueprints/` (the same source as the web gallery).
 */
type Tab = 'agents' | 'teams';

const SEGMENTS = [
  { value: 'agents', label: 'Agents' },
  { value: 'teams', label: 'Teams' },
] as const;

export const BLUEPRINTS: BlueprintData = BLUEPRINT_DATA;

function surfaceLabel(surface: string): string {
  return surface === 'desktop' ? 'Desktop · Local work' : 'Cloud';
}

function BlueprintCard({ blueprint, onCraft }: { blueprint: Blueprint; onCraft: (brief: string) => void }) {
  return (
    <Card style={styles.card} testID={`blueprint-${blueprint.slug}`}>
      <View style={styles.header}>
        <Avatar name={blueprint.name} kind="agent" decorative />
        <View style={styles.flex}>
          <Text variant="headline">{blueprint.name}</Text>
          <Text variant="footnote" tone="secondary">
            {surfaceLabel(blueprint.surface)}
            {blueprint.members ? ` · Team of ${blueprint.members.length}` : ''}
          </Text>
        </View>
      </View>
      <Text variant="subheadline">{blueprint.description}</Text>
      {blueprint.members ? (
        <Text variant="footnote" tone="secondary">
          Members: {blueprint.members.join(', ')}
        </Text>
      ) : null}
      <Text variant="footnote" tone="secondary">
        Shows: {blueprint.showcases.join(', ')}
      </Text>
      {blueprint.requirements.length > 0 ? (
        <Text variant="footnote" tone="secondary">
          Needs: {blueprint.requirements.join(' ')}
        </Text>
      ) : null}
      <Button title="Craft from it" variant="tonal" size="small" onPress={() => onCraft(blueprint.brief)} testID={`blueprint-craft-${blueprint.slug}`} />
    </Card>
  );
}

/** The gallery's list; `onCraft` receives the chosen blueprint's brief. */
export function BlueprintGallery({ onCraft }: { onCraft: (brief: string) => void }) {
  const [tab, setTab] = useState<Tab>('agents');
  return (
    <View style={styles.gallery} testID="blueprint-gallery">
      <Text variant="footnote" tone="secondary">
        Ready-made agents and teams that show what the platform can do. Craft from one to fill in its brief, then change it and generate your own agent.
      </Text>
      <SegmentedControl segments={SEGMENTS} value={tab} onChange={setTab} accessibilityLabel="Blueprint kind" />
      {BLUEPRINTS[tab].map((blueprint) => (
        <BlueprintCard key={blueprint.slug} blueprint={blueprint} onCraft={onCraft} />
      ))}
    </View>
  );
}

const styles = StyleSheet.create({
  gallery: { gap: 12 },
  card: { gap: 8 },
  header: { flexDirection: 'row', alignItems: 'center', gap: 12 },
  flex: { flex: 1 },
});
