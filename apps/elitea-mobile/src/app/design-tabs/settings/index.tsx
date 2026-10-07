import { router, useGlobalSearchParams } from 'expo-router';
import { useState } from 'react';
import { ScrollView, StyleSheet } from 'react-native';

import { useDesign } from '@/design/DesignProvider';
import { SAMPLE_ORIGIN, SAMPLE_PROJECT_ID, SAMPLE_PROJECT_NAME, SAMPLE_USER_NAME } from '@/features/designGallery/fixtures';
import { SAMPLE_USAGE } from '@/features/designGallery/secondaryFixtures';
import { UsageView } from '@/features/usage/UsageCard';
import { SettingsList } from '@/features/settings/SettingsList';
import { setSchemePreference, useSchemePreference } from '@/state/appearance';

/**
 * Development builds only: the Settings tab with sample values. It renders the real
 * `SettingsList` (D2), so a screenshot of it is the screen the user gets; Appearance is the
 * real control.
 */
export default function DesignSettingsPreview() {
  const design = useDesign();
  const scheme = useSchemePreference();
  const [workspaceIcon, setWorkspaceIcon] = useState(false);
  const params = useGlobalSearchParams<{ brand?: string }>();
  // A sample brand stands for a branded workspace: its name shows over the host.
  const [branded] = useState(() => params.brand !== undefined && params.brand !== 'zefir');
  return (
    <ScrollView
      contentInsetAdjustmentBehavior="automatic"
      style={{ backgroundColor: design.colors.background }}
      contentContainerStyle={[styles.content, { paddingHorizontal: design.space.screenGutterCompact }]}>
      <SettingsList
        userName={SAMPLE_USER_NAME}
        serverOrigin={SAMPLE_ORIGIN}
        workspaceName={branded ? 'Acme Corp AI' : null}
        logoMark={null}
        projectId={SAMPLE_PROJECT_ID}
        projectName={SAMPLE_PROJECT_NAME}
        onProject={() => {}}
        appIcon={{ workspace: workspaceIcon, onChange: setWorkspaceIcon }}
        workspaceCount={2}
        developerSignIn={false}
        scheme={scheme}
        onSchemeChange={(next) => void setSchemePreference(next)}
        lockText="When the app starts and after 5 minutes in the background"
        screenshotsAllowed={false}
        securityFooter="Security settings: set by the workspace"
        usage={<UsageView you={SAMPLE_USAGE.you} project={SAMPLE_USAGE.project} />}
        onMemory={() => router.push('/design-secondary?screen=memory')}
        onDevices={() => router.push('/design-secondary?screen=devices')}
        onAbout={() => router.push('/design-secondary?screen=about')}
        appVersion="0.1.0"
        onSwitchWorkspace={() => {}}
        onPrivacy={() => {}}
        onSupport={() => {}}
        onLogOut={() => {}}
      />
    </ScrollView>
  );
}

const styles = StyleSheet.create({
  content: { paddingVertical: 16, gap: 28, paddingBottom: 48 },
});
