import { useEffect } from 'react';
import { Platform } from 'react-native';

import { PrivacyProtection } from '@/security/privacy/privacyProtection';
import { usePolicy } from '@/security/policy';

const protection = new PrivacyProtection(Platform.OS);

/** Applies the screen-privacy policy now and whenever it changes. Mount once, at the root. */
export function usePrivacyProtection(): void {
  const { policy } = usePolicy();
  const allowScreenshots = policy.allow_screenshots;
  useEffect(() => {
    void protection.apply({ allow_screenshots: allowScreenshots });
  }, [allowScreenshots]);
}
