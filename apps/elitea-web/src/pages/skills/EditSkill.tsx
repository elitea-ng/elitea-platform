import type { ReactNode } from 'react';
import { useEffect, useMemo, useState } from 'react';

import Box from '@mui/material/Box';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { useNavigate, useParams } from '@tanstack/react-router';

import {
  exportSkill,
  isSkillValid,
  skillVersionKey,
  SkillCompareModal,
  SkillForm,
  SkillPublishControls,
  useBindSkillIconMutation,
  useSkill,
  useSkillMutations,
  type SkillIconControl,
  type SkillIconMeta,
  type SkillRecord,
  type SkillWriteInput,
} from '@/features/skills';
import { hasBackendCapability } from '@/shared/config';
import { usePermissionSet } from '@/widgets/sidebar';
import { t } from '@/shared/i18n';
import { BaseModal } from '@/shared/ui/BaseModal';
import { DeleteEntityModal } from '@/shared/ui/DeleteEntityModal';

import { useEditSkillVersionControls } from './lib/useEditSkillVersionControls';
import { useSelectedProjectId } from './lib/useSelectedProjectId';
import { SkillEditorHeader } from './SkillEditorHeader';
import { SkillTestPanel } from './SkillTestPanel';

interface EditSkillParams {
  readonly tab?: string;
  readonly skillId?: string;
  readonly version?: string;
}

export function toSkillForm(skill: SkillRecord | undefined): SkillWriteInput {
  if (!skill) return { name: '', description: '', instructions: '', tags: [] };
  const version = skill.version_details ?? skill.versions?.[0];
  return {
    name: skill.name,
    description: skill.description ?? '',
    instructions: version?.instructions ?? '',
    tags: version?.tags ?? [],
  };
}

// Re-exported for this file's own test (`./EditSkill.test.tsx`); the
// function itself lives in `features/skills/lib/skillVersionKey.ts` (see the
// import above) so `SkillCompareModal` (a feature, not a page) can use it too.
export { skillVersionKey };

/**
 * The icon a skill currently wears, read from the shape the API writes:
 * `version_details.meta.icon_meta`. It is the same path the old app's own
 * optimistic update patches, so a change here and a change there disagree
 * loudly rather than silently.
 */
function nonEmptyString(value: unknown): string | undefined {
  return typeof value === 'string' && value !== '' ? value : undefined;
}

export function toSkillIconMeta(skill: SkillRecord | undefined): SkillIconMeta | null {
  const version = skill?.version_details ?? skill?.versions?.[0];
  const iconMeta = version?.meta?.['icon_meta'] as { name?: unknown; url?: unknown } | undefined;
  const name = nonEmptyString(iconMeta?.name);
  const url = nonEmptyString(iconMeta?.url);
  // A reset writes `{}` (or an empty name/url pair); either way there is no
  // icon, and returning a half-filled meta would draw a broken image.
  return name !== undefined && url !== undefined ? { name, url } : null;
}

/** The version id an icon binds to: the one on screen, else the skill's default. */
export function skillIconVersionId(
  skill: SkillRecord | undefined,
  routeVersion: string | undefined,
): string | undefined {
  if (routeVersion !== undefined && /^[0-9]+$/.test(routeVersion)) return routeVersion;
  const id = skill?.version_details?.id ?? skill?.versions?.[0]?.id;
  return id === undefined ? undefined : String(id);
}

function downloadMarkdown(content: string, filename: string): void {
  const url = URL.createObjectURL(new Blob([content], { type: 'text/markdown' }));
  const link = document.createElement('a');
  link.href = url;
  link.download = filename;
  link.click();
  URL.revokeObjectURL(url);
}

/**
 * buildSkillIconControl returns the icon binding, or `undefined` when there is
 * nothing to bind to — no project, or a skill whose version id is not known
 * yet. `undefined` is what disables the control and explains why.
 */
function buildSkillIconControl(args: {
  readonly projectId: string | undefined;
  readonly versionId: string | undefined;
  readonly iconMeta: SkillIconMeta | null;
  readonly bind: (versionId: string, iconMeta: SkillIconMeta | null) => void;
}): SkillIconControl | undefined {
  const { projectId, versionId } = args;
  if (!projectId || !versionId) return undefined;
  return {
    projectId,
    versionId,
    iconMeta: args.iconMeta,
    onIconChange: (iconMeta) => { args.bind(versionId, iconMeta); },
  };
}

export function EditSkill(): ReactNode {
  const navigate = useNavigate();
  const params = useParams({ strict: false }) as EditSkillParams;
  const projectId = useSelectedProjectId();
  const detail = useSkill(projectId, params.skillId, params.version);
  const mutations = useSkillMutations(projectId);
  const initialValue = useMemo(() => toSkillForm(detail.data), [detail.data]);
  const [value, setValue] = useState<SkillWriteInput>(initialValue);
  const [showErrors, setShowErrors] = useState(false);
  const [deleteOpen, setDeleteOpen] = useState(false);
  const [versionOpen, setVersionOpen] = useState(false);
  const [versionName, setVersionName] = useState('');
  const [error, setError] = useState<string>();
  const permissions = usePermissionSet(projectId);
  const bindIcon = useBindSkillIconMutation(projectId ?? '');
  const versionControls = useEditSkillVersionControls({
    tab: params.tab,
    skillId: params.skillId,
    version: params.version,
    skill: detail.data,
    mutations,
    setError,
  });

  useEffect(() => setValue(initialValue), [initialValue]);

  // The icon is NOT part of the form's dirty state: it is persisted by its own
  // route the moment it is chosen, exactly as the baseline does. Folding it
  // into `value` would make picking an icon look like an unsaved edit and then
  // save it a second time through a route that does not carry it.
  const iconControl = buildSkillIconControl({
    projectId,
    versionId: skillIconVersionId(detail.data, params.version),
    iconMeta: toSkillIconMeta(detail.data),
    bind: (versionId, iconMeta) => {
      void bindIcon
        .mutateAsync({ versionId, iconMeta })
        .then(() => detail.refetch())
        .catch(() => setError(t('skills.edit.iconError', 'Failed to update the skill icon.')));
    },
  });



  // The test run POSTs `predict_llm` in its STREAMING mode
  // (`await_task_timeout: 0`, output over an `application_predict` socket
  // event). The route is served now, but that socket transport is not, so the
  // pane stays hidden — see `shared/config/backendCapabilities`.
  const canTestSkill = hasBackendCapability('llmPredictStreaming');

  const isDirty = JSON.stringify(value) !== JSON.stringify(initialValue);
  const { versions, goToVersion } = versionControls;

  const save = (): void => {
    setShowErrors(true);
    if (!isSkillValid(value) || !params.skillId) return;
    void mutations.update
      .mutateAsync({
        skillId: params.skillId,
        input: value,
        ...(params.version ? { versionId: params.version } : {}),
      })
      .catch(() => setError(t('skills.edit.saveError', 'Failed to save the skill.')));
  };

  const doExport = async (): Promise<void> => {
    if (!projectId || !params.skillId) return;
    try {
      const content = await exportSkill(projectId, params.skillId, params.version);
      downloadMarkdown(content, `${detail.data?.name || 'skill'}.md`);
    } catch {
      setError(t('skills.edit.exportError', 'Failed to export the skill.'));
    }
  };

  if (detail.isFetching && detail.data === undefined) {
    return <Typography>{t('skills.edit.loading', 'Loading skill…')}</Typography>;
  }
  if (detail.isError || !detail.data) {
    return <Typography role="alert">{t('skills.edit.loadError', 'Failed to load this skill.')}</Typography>;
  }

  return (
    <Box sx={pageSx}>
      <SkillEditorHeader
        skill={detail.data}
        versions={versions}
        activeVersion={params.version}
        isDirty={isDirty}
        isSaving={mutations.update.isPending}
        isSettingDefault={mutations.setDefault.isPending}
        onNavigateVersion={goToVersion}
        onNewVersion={() => setVersionOpen(true)}
        onSetDefault={(version) => {
          if (params.skillId) {
            void mutations.setDefault.mutateAsync({ skillId: params.skillId, versionId: version });
          }
        }}
        onCompare={versionControls.compare.onOpen}
        onRestore={versionControls.restore.onOpen}
        onDeleteVersion={versionControls.deleteVersion.onOpen}
        onSave={save}
        onDiscard={() => setValue(initialValue)}
        onDelete={() => setDeleteOpen(true)}
        onExport={() => void doExport()}
        publishing={
          <SkillPublishControls
            projectId={projectId}
            skill={detail.data}
            skillId={params.skillId}
            versionId={params.version}
            permissions={permissions}
            onPublished={() => void detail.refetch()}
          />
        }
      />
      {error && <Typography role="alert">{error}</Typography>}
      <Box sx={contentSx}>
        <Box sx={formPaneSx}>
          <SkillForm
            value={value}
            onChange={setValue}
            disabled={mutations.update.isPending}
            showErrors={showErrors}
            icon={iconControl}
          />
        </Box>
        {projectId && canTestSkill && (
          <Box sx={testPaneSx}>
            <SkillTestPanel
              projectId={projectId}
              instructions={value.instructions}
              skillName={value.name}
            />
          </Box>
        )}
      </Box>
      <DeleteEntityModal
        open={deleteOpen}
        name={detail.data.name}
        confirming={mutations.remove.isPending}
        onClose={() => setDeleteOpen(false)}
        onConfirm={() => {
          if (!params.skillId) return;
          void mutations.remove.mutateAsync({ skillId: params.skillId }).then(() =>
            navigate({ to: '/skills/$tab', params: { tab: params.tab ?? 'all' }, replace: true }),
          );
        }}
      />
      <BaseModal
        open={versionOpen}
        title={t('skills.edit.newVersionTitle', 'Create skill version')}
        onClose={() => setVersionOpen(false)}
        onConfirm={() => {
          if (!params.skillId || !versionName.trim()) return;
          void mutations.createVersion
            .mutateAsync({
              skillId: params.skillId,
              input: { name: versionName.trim(), instructions: value.instructions, tags: value.tags },
            })
            .then(() => {
              setVersionName('');
              setVersionOpen(false);
            });
        }}
        actions={{ confirming: mutations.createVersion.isPending }}
        content={
          <TextField
            fullWidth
            label={t('skills.edit.versionName', 'Version name')}
            value={versionName}
            onChange={(event) => setVersionName(event.target.value)}
          />
        }
      />
      {versionControls.compare.open && (
        <SkillCompareModal
          open={versionControls.compare.open}
          onClose={versionControls.compare.onClose}
          versions={versions}
          leftVersionKey={versionControls.activeVersionKey}
        />
      )}
      <BaseModal
        open={versionControls.restore.open}
        variant="simple"
        title={t('skills.edit.restoreVersionTitle', 'Restore this version?')}
        onClose={versionControls.restore.onClose}
        onConfirm={versionControls.restore.onConfirm}
        actions={{ confirming: versionControls.restore.confirming }}
        content={
          <Typography>
            {t(
              'skills.edit.restoreVersionBody',
              'This copies the current version’s instructions and tags onto base, overwriting base’s current content. This cannot be undone.',
            )}
          </Typography>
        }
      />
      <DeleteEntityModal
        open={versionControls.deleteVersion.open}
        name={versionControls.deleteVersion.name}
        confirming={versionControls.deleteVersion.confirming}
        onClose={versionControls.deleteVersion.onClose}
        onConfirm={versionControls.deleteVersion.onConfirm}
      />
    </Box>
  );
}

const pageSx: SxProps<Theme> = { height: '100%', display: 'flex', flexDirection: 'column' };
const contentSx: SxProps<Theme> = { flex: 1, minHeight: 0, display: 'grid', gridTemplateColumns: '1fr 1fr' };
const formPaneSx: SxProps<Theme> = (theme: Theme) => ({ overflowY: 'auto', padding: theme.spacing(3) });
const testPaneSx: SxProps<Theme> = (theme: Theme) => ({
  minWidth: 0,
  padding: theme.spacing(3),
  borderLeft: `0.0625rem solid ${theme.vars.palette.border.lines}`,
});
