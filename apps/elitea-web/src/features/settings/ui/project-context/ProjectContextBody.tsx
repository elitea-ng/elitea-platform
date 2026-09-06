/**
 * ProjectContextBody and ProjectContextToasts — extracted sub-components
 * for the project context settings page.
 *
 * Extracted from `ProjectContextContent.tsx` to keep that file under 400 lines.
 *
 * Prop budget (≤ 12 §3.5) maintained via grouped interfaces.
 */
import Box from '@mui/material/Box';
import Snackbar from '@mui/material/Snackbar';

import Alert from '@mui/material/Alert';
import { BannerMessage } from '@/shared/ui/BannerMessage';
import { BaseBtn } from '@/shared/ui/BaseBtn';
import { DrawerPage } from '@/shared/ui/settings/DrawerPage';
import { DrawerPageHeader } from '@/shared/ui/settings/DrawerPageHeader';
import { t } from '@/shared/i18n';
import { projectContextStyles } from './ProjectContext.styles';
import { EnableToggleCard } from './EnableToggleCard';
import { EditorSection } from './EditorSection';

// ---------------------------------------------------------------------------
// Grouped prop interfaces (§3.5 component-props budget)
// ---------------------------------------------------------------------------

interface ProjectInfo {
  projectId: string;
  projectName: string;
}

interface PageState {
  enabled: boolean;
  showReadOnlyBanner: boolean;
  showDisabledBanner: boolean;
  showEditorContent: boolean;
  content: string;
  mode: 'edit' | 'preview';
}

interface EditorState {
  isEditorFocused: boolean;
  showEditorControls: boolean;
  canEdit: boolean;
  isDirty: boolean;
  isSaving: boolean;
  /** Set when the reader arrived here through the empty state's "Build with AI". */
  openAiOnMount: boolean;
}

interface ContentActions {
  handleToggle: (checked: boolean) => void;
  handleContentChange: (val: string) => void;
  handleModeChange: (_e: React.SyntheticEvent, newValue: 'edit' | 'preview') => void;
  handleAIGenerated: (content: string) => void;
}

interface EditorActions {
  handleEditorBlur: (e: React.FocusEvent) => void;
  onFocus: () => void;
  onImportClick: () => void;
}

interface SaveActions {
  handleSave: () => Promise<void>;
  handleDiscard: () => void;
}

export interface ProjectContextBodyProps {
  project: ProjectInfo;
  pageState: PageState;
  editorState: EditorState;
  contentActions: ContentActions;
  editorActions: EditorActions;
  saveActions: SaveActions;
}

// ---------------------------------------------------------------------------
// Component (6 grouped props)
// ---------------------------------------------------------------------------

export function ProjectContextBody({
  project, pageState, editorState, contentActions, editorActions, saveActions,
}: ProjectContextBodyProps) {
  const s = projectContextStyles;
  return (
    /*
     * `data-testid` is load-bearing for the @visual suite, not decoration.
     *
     * `e2e/visual/routes.visual.spec.ts` used to wait on
     * `getByRole('main').or('#root > *')` for this screen. Both of those are
     * present while ProjectContext is still rendering its <CircularProgress>,
     * so the wait resolved instantly and the snapshot could capture a spinner —
     * exactly how the settings-analytics baseline became a photograph of a
     * loading state (#159/#174). This element is rendered only from a RESOLVED
     * project-context query, so it can tell loading from loaded.
     */
    <Box sx={s.root} data-testid="project-context-body">
      <DrawerPage>
        {/* The SHARED header, not a hand-rolled Box. The reference renders
          * `<DrawerPageHeader title="Project Context" showBorder />`
          * (`ProjectContextEmptyState.jsx`, `ProjectContextSavedView.jsx`), and
          * every other settings page here already does. The local copy was
          * 3.5rem tall with `1rem` of top padding against the shared header's
          * 3.8rem and vertical centring, so this one page's title sat two
          * pixels high and its rule two pixels short of every sibling tab. */}
        <DrawerPageHeader
          title={t('entities.projectContext.content.title', 'Project Context')}
          showBorder
        />

        <Box sx={s.body}>
          {/* The project avatar / name / teammates row is NOT here.
            * Production puts it in Settings › General's "General" accordion
            * (`ProjectGeneralContent.jsx`), and showed it on this tab only
            * because this port had no General tab to put it on. */}
          {pageState.showReadOnlyBanner && (
            <BannerMessage
              message={t('entities.projectContext.content.readOnlyBanner', "You don't have permission to edit this setting.")}
              variant="info"
            />
          )}

          <EnableToggleCard
            enabled={pageState.enabled}
            onToggle={contentActions.handleToggle}
            disabled={!editorState.canEdit}
          />

          {pageState.showDisabledBanner && (
            <BannerMessage
              message={t('entities.projectContext.content.disabledBanner', 'Project Context is turned off. The project background is not applied to AI responses or workflows.')}
              variant="info"
            />
          )}

          {pageState.showEditorContent && (
            <EditorSection
              projectId={project.projectId}
              content={pageState.content}
              mode={pageState.mode}
              isEditorFocused={editorState.isEditorFocused}
              showEditorControls={editorState.showEditorControls}
              canEdit={editorState.canEdit}
              onContentChange={contentActions.handleContentChange}
              onModeChange={contentActions.handleModeChange}
              onFocus={editorActions.onFocus}
              onBlur={editorActions.handleEditorBlur}
              generate={{
                onApply: contentActions.handleAIGenerated,
                openOnMount: editorState.openAiOnMount,
              }}
              onImportClick={editorActions.onImportClick}
            />
          )}

          <Box sx={s.actions}>
            <BaseBtn
              variant="contained"
              color="primary"
              disabled={!editorState.canEdit || !editorState.isDirty || editorState.isSaving}
              onClick={() => void saveActions.handleSave()}
            >
              {t('entities.projectContext.content.save', 'Save')}
            </BaseBtn>
            <BaseBtn
              variant="secondary"
              color="secondary"
              disabled={!editorState.canEdit || !editorState.isDirty}
              onClick={() => saveActions.handleDiscard()}
            >
              {t('entities.projectContext.content.discard', 'Discard')}
            </BaseBtn>
          </Box>
        </Box>
      </DrawerPage>
    </Box>
  );
}

export interface ProjectContextToastsProps {
  showSaveToast: boolean;
  showErrorToast: boolean;
  onCloseSave: () => void;
  onCloseError: () => void;
}

export function ProjectContextToasts({
  showSaveToast,
  showErrorToast,
  onCloseSave,
  onCloseError,
}: ProjectContextToastsProps) {
  return (
    <>
      <Snackbar
        open={showSaveToast}
        autoHideDuration={3000}
        onClose={onCloseSave}
        anchorOrigin={{ vertical: 'bottom', horizontal: 'center' }}
      >
        <Alert onClose={onCloseSave} severity="success" variant="filled">
          {t('entities.projectContext.content.saveSuccess', 'Project Context saved successfully')}
        </Alert>
      </Snackbar>
      <Snackbar
        open={showErrorToast}
        autoHideDuration={3000}
        onClose={onCloseError}
        anchorOrigin={{ vertical: 'bottom', horizontal: 'center' }}
      >
        <Alert onClose={onCloseError} severity="error" variant="filled">
          {t('entities.projectContext.content.saveError', 'Failed to save Project Context')}
        </Alert>
      </Snackbar>
    </>
  );
}
