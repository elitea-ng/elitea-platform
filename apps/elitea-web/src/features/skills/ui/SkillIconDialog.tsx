/**
 * SkillIconDialog — the skill's icon picker.
 *
 * It REUSES `shared/ui/IconPickerDialog`, the dialog `ProjectIconDialog` already
 * uses; the old app's `components/SelectIconDialog.jsx` is deliberately not
 * ported a second time. Only the data differs: the skill icon endpoints instead
 * of the project ones, and a bind that writes the version's `meta.icon_meta`.
 *
 * WHY THE SELECTION NEEDS THE FULL META, not just the name. The bind (PUT)
 * takes `{name, url}`, so choosing an icon from the gallery has to hand back
 * the ROW, not its name. The gallery rows carry both, which is why this wrapper
 * looks the name up in the two lists it already holds rather than asking the
 * parent to reconstruct a url.
 */
import type { ReactNode } from 'react';
import { useCallback, useMemo } from 'react';

import { useGetApplicationDefaultIcons } from '@/shared/api/generated/applications/applications';
import type { DefaultIcon } from '@/shared/api/generated/model/defaultIcon.zod';
import { IconPickerDialog, type PickableIcon, type UploadDimensions } from '@/shared/ui/IconPickerDialog';

import {
  useDeleteSkillIconMutation,
  useSkillIconsQuery,
  useUploadSkillIconMutation,
  type SkillIconMeta,
} from '../api/skillIconApi';

/**
 * What the server stores for a skill icon (`eliteacore/skill_icon.go`): the
 * raster formats below, at most 512 KB. The picker refuses anything else with a
 * message instead of sending it (#2107); the server enforces the same rules.
 */
const SKILL_ICON_EXTENSIONS: readonly string[] = ['.png', '.jpg', '.jpeg', '.gif', '.webp', '.bmp', '.ico'];
const SKILL_ICON_MAX_BYTES = 512 * 1024;

export interface SkillIconDialogProps {
  readonly open: boolean;
  readonly onClose: () => void;
  readonly projectId: string;
  /** The skill VERSION the icon binds to. */
  readonly versionId: string;
  readonly skillName: string;
  readonly selectedIcon?: SkillIconMeta | null | undefined;
  /** Called with the chosen icon, or `null` to reset the skill to the default. */
  readonly onIconSelect: (iconMeta: SkillIconMeta | null) => void;
}

export function SkillIconDialog({
  open,
  onClose,
  projectId,
  versionId,
  skillName,
  selectedIcon,
  onIconSelect,
}: SkillIconDialogProps): ReactNode {
  const { data: defaultIconsResponse, isLoading: loadingDefault } = useGetApplicationDefaultIcons(projectId, {
    query: { enabled: open && !!projectId },
  });
  const defaultIcons = useMemo(
    () => (defaultIconsResponse?.data ?? []) as DefaultIcon[],
    [defaultIconsResponse],
  );

  const { data: iconsResponse, isLoading: loadingIcons } = useSkillIconsQuery(projectId, {
    enabled: open && !!projectId,
  });
  const uploadedIcons = useMemo(() => iconsResponse?.rows ?? [], [iconsResponse]);

  const uploadMutation = useUploadSkillIconMutation(projectId);
  const deleteMutation = useDeleteSkillIconMutation(projectId);

  const handleSelect = useCallback(
    (name: string | null) => {
      if (name === null) {
        onIconSelect(null);
        return;
      }
      const chosen: PickableIcon | undefined =
        uploadedIcons.find((icon) => icon.name === name) ??
        defaultIcons.find((icon) => icon.name === name);
      // A name with no url cannot be bound: the server requires both. Falling
      // back to the name alone would write an icon_meta that renders as a
      // broken image everywhere it is read.
      if (!chosen?.url) return;
      onIconSelect({ name: chosen.name, url: chosen.url });
    },
    [defaultIcons, uploadedIcons, onIconSelect],
  );

  const handleUpload = useCallback(
    async (file: File, dimensions: UploadDimensions): Promise<void> => {
      // The upload binds the icon to the version in the SAME request (the
      // route's optional trailing segment), so a fresh upload is worn
      // immediately rather than needing a second PUT the user never made.
      const uploaded = await uploadMutation.mutateAsync({ file, ...dimensions, versionId });
      onIconSelect({ name: uploaded.name, url: uploaded.url });
      onClose();
    },
    [uploadMutation, versionId, onIconSelect, onClose],
  );

  const handleDelete = useCallback(
    async (name: string): Promise<void> => {
      await deleteMutation.mutateAsync(name);
    },
    [deleteMutation],
  );

  return (
    <IconPickerDialog
      open={open}
      onClose={onClose}
      selectedIcon={selectedIcon}
      placeholderName={skillName}
      defaultIcons={defaultIcons}
      loadingDefaultIcons={loadingDefault}
      uploadedIcons={uploadedIcons}
      loadingUploadedIcons={loadingIcons}
      onSelectIcon={handleSelect}
      onUpload={handleUpload}
      onDeleteIcon={handleDelete}
      uploadLimits={{ extensions: SKILL_ICON_EXTENSIONS, maxBytes: SKILL_ICON_MAX_BYTES }}
    />
  );
}
