// @ts-nocheck
/**
 * ProjectContext — page-level composition for the Project Context settings
 * tab (Settings > Project Params > Project Context).
 * Ported from `apps/elitea-ui/src/[fsd]/features/settings/ui/project-context/ProjectContextContent.jsx`.
 *
 * Uses:
 *  - Generated `useGetProjectContext` for context retrieval
 *  - Raw `updateProjectContext` (PUT) via `useMutation` for context updates
 *  - `useUpdateProjectInfo` mutation for icon persistence
 *  - `GenerateProjectContextButton` for AI-generated drafts
 *  - `EnableToggleCard` (extracted) for the enable/disable toggle
 *  - `ProjectParamsHeader` for the project avatar + info row
 *  - `CodeMirrorEditor` for editing (no markdown extension — not installed)
 *  - `Markdown` for preview mode
 *  - `t()` for all text
 *
 * Permissions (spec §9.3): `PERMISSIONS.projectContext.view` gates the
 * entire page; `PERMISSIONS.projectContext.edit` gates save/discard/generate
 * buttons. Without `view`, the user sees a "No access" message.
 */
import Box from '@mui/material/Box';
import CircularProgress from '@mui/material/CircularProgress';

import { useMutation, useQueryClient } from '@tanstack/react-query';

import { BannerMessage } from '@/shared/ui/BannerMessage';
import { projectContextFeature } from '@/features/settings';
import { DrawerPage } from '@/shared/ui/settings/DrawerPage';
import { DrawerPageHeader } from '@/shared/ui/settings/DrawerPageHeader';
import { t } from '@/shared/i18n';
import {
  updateProjectContext,
  useGetProjectContext,
} from '@/shared/api/generated/applications/applications';
import { PERMISSIONS } from '@/shared/lib/permissions';
import { usePermissionSet } from '@/widgets/sidebar';
import { useCallback, useEffect, useRef, useState } from 'react';

const {
  ProjectContextBody,
  ProjectContextEmptyState,
  ProjectContextToasts,
  hasSavedProjectContext,
  projectContextStyles,
} = projectContextFeature;

/* ── constants ─────────────────────────────────────────────────────────── */

const MAX_CHARS = 2500;

/* ── helper: derive show conditions ───────────────────────────────────── */

interface ProjectContextShowFlags {
  readonly showReadOnlyBanner: boolean;
  readonly showDisabledBanner: boolean;
  readonly showEditorContent: boolean;
  readonly showEditorControls: boolean;
}

function deriveShowFlags(canEdit: boolean, enabled: boolean, content: string): ProjectContextShowFlags {
  return {
    showReadOnlyBanner: !canEdit,
    showDisabledBanner: !enabled && Boolean(content.trim()),
    // `|| !canEdit`: a view-only user always sees the (possibly empty,
    // read-only) editor section — old-app parity, ProjectContextContent.jsx:57.
    showEditorContent: enabled || Boolean(content.trim()) || !canEdit,
    showEditorControls: enabled && canEdit,
  };
}

/* ── props ─────────────────────────────────────────────────────────────── */

export interface ProjectContextProps {
  projectId: string;
  projectName: string;
  /**
   * Whether the user has `view` permission for project context. Overrides
   * the real permission check below when explicitly supplied (e.g. tests);
   * omit to compute it from `PERMISSIONS.projectContext.view`.
   */
  canView?: boolean;
  /**
   * Whether the user has `edit` permission for project context. Overrides
   * the real permission check below when explicitly supplied (e.g. tests);
   * omit to compute it from `PERMISSIONS.projectContext.edit`.
   */
  canEdit?: boolean;
}

/* ── component ─────────────────────────────────────────────────────────── */

export function ProjectContext({
  projectId,
  projectName,
  canView: canViewProp,
  canEdit: canEditProp,
}: ProjectContextProps) {
  /* ── hooks (must come before any early return) ───────────────────── */

  const fileInputRef = useRef<HTMLInputElement>(null);
  const queryClient = useQueryClient();

  /* ── permissions (spec §9.3): real check, not a default-true prop ── */

  const permissionSet = usePermissionSet(projectId);
  const canView = canViewProp ?? permissionSet.has(PERMISSIONS.projectContext.view);
  const canEdit = canEditProp ?? permissionSet.has(PERMISSIONS.projectContext.edit);

  /* ── project context query ──────────────────────────────────────── */

  const { data: ctxResponse, isLoading, isError } = useGetProjectContext(projectId, {
    query: { enabled: !!projectId && canView },
  });

  /* ── mutations ──────────────────────────────────────────────────── */

  const saveMutation = useMutation({
    mutationFn: ({ content, enabled }: { content: string; enabled: boolean }) =>
      updateProjectContext(projectId, { content, enabled }),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: [`/elitea_core/project_context/prompt_lib/${projectId}/project-context`] });
    },
  } as { mutateAsync: (args: { content: string; enabled: boolean }) => Promise<unknown> });

  /* ── local state ────────────────────────────────────────────────── */

  const [isSaving, setIsSaving] = useState(false);
  const [isEditorFocused, setIsEditorFocused] = useState(false);
  const [showSaveToast, setShowSaveToast] = useState(false);
  const [showErrorToast, setShowErrorToast] = useState(false);
  const [content, setContent] = useState('');
  const [enabled, setEnabled] = useState(true);
  const [mode, setMode] = useState<'edit' | 'preview'>('edit');
  const [isDirty, setIsDirty] = useState(false);
  /*
   * THE EMPTY STATE IS A SCREEN, NOT A PLACEHOLDER INSIDE THE EDITOR.
   *
   * The reference splits this tab in two (`ProjectContextContent.jsx`): a
   * project with saved content gets the editor, a project without gets a
   * centred invitation, and "Create" / "Build with AI" route to the editor.
   * That router hop has no equivalent here — this app serves the whole tab
   * from one route — so the same two-screen shape is held in state. It starts
   * false on every mount, which is what makes the empty state the DEFAULT for
   * a project with nothing saved, exactly as production behaves.
   */
  const [isEditing, setIsEditing] = useState(false);
  const [openAiOnMount, setOpenAiOnMount] = useState(false);

  /* ── server data ────────────────────────────────────────────────── */

  const serverData = ctxResponse?.data;
  const isProjectContext =
    serverData && typeof serverData === 'object' && 'content' in serverData;

  useEffect(() => {
    if (isProjectContext && serverData !== undefined) {
      setContent(serverData.content ?? '');
      setEnabled(serverData.enabled ?? true);
      setIsDirty(false);
    }
  }, [isProjectContext, serverData]);

  /* ── event handlers ─────────────────────────────────────────────── */

  const handleContentChange = useCallback((val: string) => {
    setContent(val);
    setIsDirty(true);
  }, []);

  const handleModeChange = useCallback(
    (_e: React.SyntheticEvent, newValue: 'edit' | 'preview') => {
      setMode(newValue);
    },
    [],
  );

  const handleToggle = useCallback((checked: boolean) => {
    setEnabled(checked);
    setIsDirty(true);
  }, []);

  const handleFileUpload = useCallback((e: React.ChangeEvent<HTMLInputElement>) => {
    const file = e.target.files?.[0];
    if (!file) return;
    const reader = new FileReader();
    reader.onload = (ev) => {
      const result = (ev.target?.result as string) ?? '';
      const text = String(result).replace(/\r\n/g, '\n').replace(/\r/g, '\n');
      if (text.length > MAX_CHARS) {
        console.warn(t('entities.projectContext.content.fileTooLarge', 'File content exceeds 2500 characters'));
        return;
      }
      setContent(text);
      setIsDirty(true);
    };
    reader.readAsText(file);
    e.target.value = '';
  }, []);

  const handleSave = useCallback(async () => {
    try {
      setIsSaving(true);
      await saveMutation.mutateAsync({ content, enabled });
      setIsDirty(false);
      setShowSaveToast(true);
    } catch {
      setShowErrorToast(true);
    } finally {
      setIsSaving(false);
    }
  }, [content, enabled, saveMutation]);

  const handleDiscard = useCallback(() => {
    if (!isProjectContext || serverData === undefined) return;
    setContent(serverData.content ?? '');
    setEnabled(serverData.enabled ?? true);
    setIsDirty(false);
  }, [isProjectContext, serverData]);

  const handleCreate = useCallback(() => {
    setIsEditing(true);
  }, []);

  const handleBuildWithAi = useCallback(() => {
    setIsEditing(true);
    setOpenAiOnMount(true);
  }, []);

  const handleAIGenerated = useCallback((generatedContent: string) => {
    setContent(generatedContent);
    setIsDirty(true);
  }, []);

  const handleEditorBlur = useCallback((e: React.FocusEvent) => {
    if (!e.currentTarget.contains(e.relatedTarget)) {
      setIsEditorFocused(false);
    }
  }, []);

  const handleImportClick = useCallback(() => {
    fileInputRef.current?.click();
  }, []);

  const handleCloseErrorToast = useCallback(() => setShowErrorToast(false), []);
  const handleCloseSaveToast = useCallback(() => setShowSaveToast(false), []);

  /* ── permissions early return ───────────────────────────────────── */

  if (!canView) {
    return (
      <Box sx={projectContextStyles.root}>
        <DrawerPage>
          <BannerMessage
            message={t('entities.projectContext.content.noAccess', 'You do not have permission to view this setting.')}
            variant="info"
          />
        </DrawerPage>
      </Box>
    );
  }

  /* ── derived state ──────────────────────────────────────────────── */

  const showFlags = deriveShowFlags(canEdit, enabled, content);
  const { showReadOnlyBanner, showDisabledBanner, showEditorContent, showEditorControls } = showFlags;

  /* ── loading state ──────────────────────────────────────────────── */

  const s = projectContextStyles;

  if (isLoading) {
    return (
      <Box sx={s.loader}>
        <CircularProgress size={32} />
      </Box>
    );
  }

  /* ── failed-load state ──────────────────────────────────────────── */

  /*
   * Without this branch a FAILED query fell through to the editor below with
   * `content` still at its initial `''` — an empty, editable, saveable box that
   * looks exactly like a project whose context is genuinely empty, and whose
   * Save would overwrite the real content with nothing.
   *
   * It also mattered for the @visual suite: the spec's landmark is
   * `project-context-body`, and a landmark that a failed load can still produce
   * cannot discriminate loaded from broken — which is the whole defect #174
   * describes for the old `getByRole('main')` wait.
   */
  if (isError) {
    return (
      <Box sx={s.root}>
        <DrawerPage>
          <BannerMessage
            message={t('entities.projectContext.content.loadFailed', 'Failed to load Project Context.')}
            variant="error"
          />
        </DrawerPage>
      </Box>
    );
  }

  /*
   * THE EMPTY STATE — the branch this port never had. See
   * `features/settings/lib/project-context/hasSavedContent.ts` for why the
   * test reads the SERVER's content and not the editor's buffer.
   */
  if (!hasSavedProjectContext(serverData) && !isEditing) {
    return (
      <Box sx={s.root}>
        <DrawerPage>
          <DrawerPageHeader
            title={t('entities.projectContext.content.title', 'Project Context')}
            showBorder
          />
          <ProjectContextEmptyState
            canEdit={canEdit}
            onCreate={handleCreate}
            onBuildWithAi={handleBuildWithAi}
          />
        </DrawerPage>
      </Box>
    );
  }

  return (
    <>
      <ProjectContextBody
        project={{ projectId, projectName }}
        pageState={{ enabled, showReadOnlyBanner, showDisabledBanner, showEditorContent, content, mode }}
        editorState={{ isEditorFocused, showEditorControls, canEdit, isDirty, isSaving, openAiOnMount }}
        contentActions={{ handleToggle, handleContentChange, handleModeChange, handleAIGenerated }}
        editorActions={{ handleEditorBlur, onFocus: () => setIsEditorFocused(true), onImportClick: handleImportClick }}
        saveActions={{ handleSave, handleDiscard }}
      />
      <input
        ref={fileInputRef}
        type="file"
        accept=".md,text/markdown"
        style={{ display: 'none' }}
        onChange={handleFileUpload}
      />
      <ProjectContextToasts
        showSaveToast={showSaveToast}
        showErrorToast={showErrorToast}
        onCloseSave={handleCloseSaveToast}
        onCloseError={handleCloseErrorToast}
      />
    </>
  );
}
