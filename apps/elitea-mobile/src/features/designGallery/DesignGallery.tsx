import { router, Stack } from 'expo-router';
import { useState } from 'react';
import { ScrollView, StyleSheet, Switch, View } from 'react-native';

import { Mascot } from '@/components/Mascot';
import { DesignProvider, useDesign } from '@/design/DesignProvider';
import type { BrandInput } from '@/design/brand';
import type { SchemePreference } from '@/design/resolve';
import type { TypeRole } from '@/design/typography';
import {
  ActionCard,
  ArrowButton,
  Avatar,
  Badge,
  Button,
  Card,
  Chip,
  ChipRow,
  EmptyState,
  GlassSurface,
  GradientBackground,
  haptic,
  headerActionOptions,
  Icon,
  IconButton,
  IconTile,
  ICONS,
  largeTitleOptions,
  ListRow,
  ListSection,
  searchBarOptions,
  SegmentedControl,
  Text,
  type IconName,
} from '@/ui';

/**
 * The design gallery (development builds only, `/design-gallery`): every kit component in the
 * chosen scheme and brand, so a change to the tokens or the kit can be checked, and screenshot,
 * without a server. The native chrome (navigation bar, search, bar buttons) follows the device
 * appearance; the content below follows the gallery's own scheme and brand choice.
 *
 * Brands are sample packs run through the real correction (§3.3): a weak accent is corrected,
 * never dropped.
 */
export const GALLERY_BRANDS = {
  zefir: null,
  magenta: { productDefault: false, lightAccent: '#C428DD', darkAccent: '#6AE8FA' },
  green: { productDefault: false, lightAccent: '#14B8A6', darkAccent: '#14B8A6' },
  lemon: { productDefault: false, lightAccent: '#FFD400', darkAccent: '#FFD400' },
} as const satisfies Record<string, BrandInput | null>;

export type GalleryBrand = keyof typeof GALLERY_BRANDS;

export function parseGalleryParams(params: { scheme?: string | string[]; brand?: string | string[] }): {
  scheme: SchemePreference;
  brand: GalleryBrand;
} {
  const one = (v?: string | string[]) => (Array.isArray(v) ? v[0] : v);
  const scheme = one(params.scheme);
  const brand = one(params.brand);
  return {
    scheme: scheme === 'light' || scheme === 'dark' ? scheme : 'system',
    brand: brand !== undefined && brand in GALLERY_BRANDS ? (brand as GalleryBrand) : 'zefir',
  };
}

const ROLES: TypeRole[] = ['display', 'title1', 'title2', 'title3', 'headline', 'body', 'callout', 'subheadline', 'footnote', 'caption', 'label', 'code'];

export function DesignGallery({ initialScheme = 'system', initialBrand = 'zefir' }: { initialScheme?: SchemePreference; initialBrand?: GalleryBrand }) {
  const chrome = useDesign();
  const [scheme, setScheme] = useState<SchemePreference>(initialScheme);
  const [brand, setBrand] = useState<GalleryBrand>(initialBrand);
  const [filter, setFilter] = useState('');
  return (
    <>
      <Stack.Screen
        options={{
          title: 'Design gallery',
          headerShown: true,
          ...largeTitleOptions(chrome),
          ...searchBarOptions(chrome, { placeholder: 'Filter sections', onChange: setFilter }),
          ...headerActionOptions(
            [
              { id: 'bell', icon: 'bell', label: 'Notifications', accessibilityLabel: 'Notifications, 3 unread', badge: 3, onPress: () => haptic('tabOrChipChange') },
              { id: 'sheet', icon: 'grid', label: 'Open a sheet', onPress: () => router.push('/design-sheet') },
            ],
            chrome,
          ),
        }}
      />
      <DesignProvider scheme={scheme} brand={GALLERY_BRANDS[brand]}>
        <GalleryBody scheme={scheme} setScheme={setScheme} brand={brand} setBrand={setBrand} filter={filter} />
      </DesignProvider>
    </>
  );
}

function GalleryBody({
  scheme,
  setScheme,
  brand,
  setBrand,
  filter,
}: {
  scheme: SchemePreference;
  setScheme: (s: SchemePreference) => void;
  brand: GalleryBrand;
  setBrand: (b: GalleryBrand) => void;
  filter: string;
}) {
  const design = useDesign();
  const { colors } = design;
  const [toggle, setToggle] = useState(true);
  const [chip, setChip] = useState('all');
  const show = (name: string) => filter.trim() === '' || name.toLowerCase().includes(filter.trim().toLowerCase());
  return (
    <ScrollView
      contentInsetAdjustmentBehavior="automatic"
      keyboardDismissMode="on-drag"
      style={{ backgroundColor: colors.background }}
      contentContainerStyle={[styles.content, { paddingHorizontal: design.space.screenGutterCompact }]}>
      <ListSection title="Preview" footer={`Accent ${colors.accent} on ${colors.accentOn}${design.branded ? ' (corrected brand)' : ' (Zefir rose)'}`}>
        <View style={styles.pad}>
          <SegmentedControl
            accessibilityLabel="Preview scheme"
            value={scheme}
            onChange={setScheme}
            segments={[
              { value: 'system', label: 'System' },
              { value: 'light', label: 'Light', icon: 'light' },
              { value: 'dark', label: 'Dark', icon: 'dark' },
            ]}
          />
        </View>
        <View style={styles.pad}>
          <SegmentedControl
            accessibilityLabel="Preview brand"
            value={brand}
            onChange={setBrand}
            segments={[
              { value: 'zefir', label: 'Zefir' },
              { value: 'magenta', label: 'Magenta' },
              { value: 'green', label: 'Green' },
              { value: 'lemon', label: 'Lemon' },
            ]}
          />
        </View>
        <ListRow icon="grid" title="Tab shell preview" subtitle="Native tabs, large titles, header search" onPress={() => router.push('/design-tabs/chats')} />
        <ListRow icon="chats" title="Conversation preview" subtitle="The composer on the keyboard" onPress={() => router.push('/design-composer')} />
      </ListSection>

      {show('home cards gradient glass') ? (
        <Section title="Home: gradient, glass, action cards">
          <View style={[styles.gradientFrame, { borderRadius: design.radius.card }]}>
            <GradientBackground style={styles.gradient}>
              <View style={styles.greetingRow}>
                <Avatar name="Alex Smith" size={36} />
                <IconButton icon="bell" badge={3} accessibilityLabel="Notifications, 3 unread" onPress={() => haptic('tabOrChipChange')} />
              </View>
              <Text variant="display" accessibilityRole="header">
                <Text variant="display" tone="accent">
                  Hello, Alex
                </Text>
                {'\n'}What can I help with?
              </Text>
              <ChipRow>
                <Chip label="Agent: Research" menu selected onPress={() => {}} />
                <Chip label="Talk" icon="mic" iconOnly onPress={() => {}} />
                <Chip label="Ask about a photo" icon="image" iconOnly onPress={() => {}} />
                <Chip label="Attach a file" icon="attach" iconOnly onPress={() => {}} />
              </ChipRow>
              <View style={[styles.grid, design.accessibilityLayout ? styles.gridStacked : null]}>
                <ActionCard title="New chat" icon="newChat" accessibilityHint="Starts a new chat" onPress={() => haptic('send')} />
                <ActionCard title="Talk" icon="mic" accessibilityHint="Starts dictation" onPress={() => haptic('voiceStart')} />
              </View>
              <View style={[styles.grid, design.accessibilityLayout ? styles.gridStacked : null]}>
                <ActionCard title="Ask about a photo" icon="image" accessibilityHint="Attaches a photo to a new chat" onPress={() => {}} />
                <ActionCard title="Continue: Q3 planning" icon="regenerate" accessibilityHint="Opens the last chat" onPress={() => {}} />
              </View>
              <GlassSurface radius={design.radius.composer} floating style={styles.composer}>
                <Icon name="plus" size={22} />
                <Text variant="body" tone="tertiary" style={styles.flex}>
                  Message Research…
                </Text>
                <View style={[styles.send, { backgroundColor: colors.accent }]}>
                  <Icon name="send" size={18} color={colors.accentOn} weight="bold" />
                </View>
              </GlassSurface>
            </GradientBackground>
          </View>
        </Section>
      ) : null}

      {show('buttons') ? (
        <Section title="Buttons">
          <Button title="Continue with SSO" haptic="send" onPress={() => {}} />
          <Button title="Enter server address" variant="tonal" onPress={() => {}} />
          <Button title="Scan workspace QR code" icon="qr" onPress={() => {}} />
          <View style={styles.row}>
            <Button title="Plain" variant="plain" size="small" onPress={() => {}} />
            <Button title="Retry" variant="tonal" size="small" icon="regenerate" onPress={() => {}} />
            <Button title="Delete" variant="destructivePlain" size="small" onPress={() => {}} />
          </View>
          <Button title="Log out & wipe" variant="destructive" onPress={() => {}} />
          <Button title="Signing in…" loading onPress={() => {}} />
          <Button title="Disabled" disabled onPress={() => {}} />
        </Section>
      ) : null}

      {show('icon buttons chips') ? (
        <Section title="Icon buttons and chips">
          <View style={styles.row}>
            <IconButton icon="bell" badge={12} accessibilityLabel="Notifications, 12 unread" onPress={() => {}} />
            <IconButton icon="newChat" variant="tonal" accessibilityLabel="New chat" onPress={() => {}} />
            <IconButton icon="send" variant="filled" accessibilityLabel="Send" haptic="send" onPress={() => {}} />
            <IconButton icon="more" variant="plain" accessibilityLabel="More" onPress={() => {}} />
            <IconButton icon="like" variant="plain" selected accessibilityLabel="Like" onPress={() => {}} />
          </View>
          <ChipRow>
            {['all', 'unread', 'pinned'].map((c) => (
              <Chip key={c} label={c[0].toUpperCase() + c.slice(1)} selected={chip === c} onPress={() => setChip(c)} />
            ))}
          </ChipRow>
        </Section>
      ) : null}

      {show('lists rows settings') ? (
        <Section title="Lists" bare>
          <ListSection title="Workspace" footer="Set by acme.elitea.ai">
            <ListRow title="acme.elitea.ai" subtitle="Acme Corp AI" leading={<IconTile icon="workspace" size={32} tone="secondary" />} onPress={() => {}} />
            <ListRow icon="project" title="Project" value="Marketing" onPress={() => {}} />
            <ListRow icon="lock" title="Face ID lock" trailing={<Switch value={toggle} onValueChange={setToggle} trackColor={{ true: colors.accent }} />} />
            <ListRow title="Personal" selected onPress={() => {}} />
            <ListRow title="Engineering" selected={false} onPress={() => {}} />
          </ListSection>
          <ListSection>
            <ListRow title="Log out & wipe" destructive onPress={() => {}} />
          </ListSection>
        </Section>
      ) : null}

      {show('cards history') ? (
        <Section title="Cards">
          <Card>
            <View style={styles.cardRow}>
              <Avatar kind="agent" name="Research" mascot size={36} />
              <View style={styles.flex}>
                <Text variant="headline" numberOfLines={1}>
                  Make my website after the launch
                </Text>
                <Text variant="footnote" tone="secondary">
                  Research · 2h
                </Text>
              </View>
              <ArrowButton size={36} />
            </View>
          </Card>
          <Card glass>
            <Text variant="headline">Glass card</Text>
            <Text variant="callout" tone="secondary">
              Liquid Glass on iOS 26, a tonal surface on Android, opaque with Reduce Transparency.
            </Text>
          </Card>
        </Section>
      ) : null}

      {show('avatars mascot') ? (
        <Section title="Avatars and mascot">
          <View style={styles.row}>
            {['Alex Smith', 'Bob Zefir', 'Chen Li', 'Dana Ortiz', 'Eve Moss', 'Femi Ade'].map((n) => (
              <Avatar key={n} name={n} size={36} />
            ))}
          </View>
          <View style={styles.row}>
            <Avatar kind="agent" name="Research" mascot size={48} />
            <Avatar kind="agent" name="HR helper" size={48} />
            <Mascot variant="bust" size={96} decorative />
            <Mascot variant="full" size={80} decorative />
          </View>
          <View style={styles.row}>
            <Badge count={3} />
            <Badge count={120} />
          </View>
        </Section>
      ) : null}

      {show('empty state') ? (
        <Section title="Empty state">
          <View style={[styles.gradientFrame, { borderRadius: design.radius.card }]}>
            <GradientBackground>
              <EmptyState title="Ask me anything" body="Start a chat or try your voice." action={{ title: 'New chat', onPress: () => {} }} />
            </GradientBackground>
          </View>
        </Section>
      ) : null}

      {show('type typography') ? (
        <Section title="Type ramp">
          {ROLES.map((role) => (
            <Text key={role} variant={role} numberOfLines={1}>
              {role} · {design.typography[role].fontSize}
            </Text>
          ))}
        </Section>
      ) : null}

      {show('icons symbols') ? (
        <Section title="Icons">
          <View style={styles.icons}>
            {(Object.keys(ICONS) as IconName[]).map((name) => (
              <View key={name} style={styles.iconCell}>
                <Icon name={name} size={22} tone="primary" />
                <Text variant="caption" tone="secondary" numberOfLines={1}>
                  {name}
                </Text>
              </View>
            ))}
          </View>
        </Section>
      ) : null}
    </ScrollView>
  );
}

function Section({ title, children, bare = false }: { title: string; children: React.ReactNode; bare?: boolean }) {
  return (
    <View style={styles.section}>
      <Text variant="title3" heading>
        {title}
      </Text>
      <View style={bare ? styles.bare : styles.stack}>{children}</View>
    </View>
  );
}

const styles = StyleSheet.create({
  content: { paddingVertical: 16, gap: 28, paddingBottom: 64 },
  pad: { padding: 12 },
  section: { gap: 12 },
  stack: { gap: 12 },
  bare: { gap: 20 },
  row: { flexDirection: 'row', flexWrap: 'wrap', alignItems: 'center', gap: 12 },
  flex: { flex: 1 },
  gradientFrame: { overflow: 'hidden', borderCurve: 'continuous' },
  gradient: { padding: 16, gap: 16 },
  greetingRow: { flexDirection: 'row', justifyContent: 'space-between', alignItems: 'center' },
  grid: { flexDirection: 'row', gap: 12 },
  gridStacked: { flexDirection: 'column' },
  composer: { flexDirection: 'row', alignItems: 'center', gap: 10, minHeight: 52, paddingHorizontal: 12, paddingVertical: 8 },
  send: { width: 36, height: 36, borderRadius: 18, alignItems: 'center', justifyContent: 'center' },
  cardRow: { flexDirection: 'row', alignItems: 'center', gap: 12 },
  icons: { flexDirection: 'row', flexWrap: 'wrap', gap: 8 },
  iconCell: { width: 76, alignItems: 'center', gap: 4, paddingVertical: 6 },
});
