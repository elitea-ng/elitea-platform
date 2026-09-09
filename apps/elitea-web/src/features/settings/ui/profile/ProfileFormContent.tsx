/**
 * ProfileFormContent — composes all profile section components.
 */
import Box from '@mui/material/Box';

import { useFormikAutoSaveOnBlur } from '@/shared/lib/hooks/useFormikAutoSaveOnBlur';

import { ProfileContextManagement } from './ProfileContextManagement';
import { ProfilePersonalization } from './ProfilePersonalization';
import { ProfileUserInfo } from './ProfileUserInfo';
import { VoicePersonalizationSection } from './voice-config/VoicePersonalizationSection';
import { SoundNotificationSection } from './SoundNotificationSection';

export interface ProfileFormContentProps {
  /** Currently-selected project id — threaded down from the route. */
  projectId: string;
  name: string;
  avatar: string;
  email: string;
  isFetching: boolean;
  modelList: Array<{
    name: string;
    project_id: string;
    default?: boolean;
    display_name?: string;
  }>;
}

export function ProfileFormContent({
  projectId,
  name,
  avatar,
  email,
  isFetching,
  modelList,
}: ProfileFormContentProps) {
  const { onBlur, requestSubmit } = useFormikAutoSaveOnBlur();

  return (
    <Box
      sx={styles.wrapper}
      onBlur={onBlur}
    >
      <Box sx={styles.container}>
        <ProfileUserInfo name={name} avatar={avatar} email={email} isFetching={isFetching} />
        <ProfilePersonalization onAutoSaveRequested={requestSubmit} />
        <ProfileContextManagement modelList={modelList} onAutoSaveRequested={requestSubmit} projectId={projectId} />
        <VoicePersonalizationSection projectId={projectId} />
        <SoundNotificationSection />
      </Box>
    </Box>
  );
}

const styles = {
  wrapper: {
    display: 'flex',
    justifyContent: 'center',
    width: '100%',
  },
  container: {
    display: 'flex',
    flexDirection: 'column',
    gap: '0.5rem',
    padding: '1.5rem',
    maxWidth: '50rem',
    width: '100%',
  },
};
