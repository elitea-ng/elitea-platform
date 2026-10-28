/**
 * The host's own menus — the menu bar, the menu-bar (tray) item and its
 * activity lines — in the app's language. The keys are the host's
 * (`src-tauri/src/labels.rs`, `KEYS`), which checks them and keeps English
 * for any it does not get. `{tool}` stays in place: the host puts the tool's
 * name there.
 */
import { t } from '@/shared/i18n';

export function nativeMenuLabels(): Record<string, string> {
  return {
    about: t('desktop.nativeMenu.about', 'About Elitea'),
    services: t('desktop.nativeMenu.services', 'Services'),
    hide: t('desktop.nativeMenu.hide', 'Hide Elitea'),
    hide_others: t('desktop.nativeMenu.hideOthers', 'Hide Others'),
    show_all: t('desktop.nativeMenu.showAll', 'Show All'),
    quit: t('desktop.nativeMenu.quit', 'Quit Elitea'),
    file: t('desktop.nativeMenu.file', 'File'),
    new_thread: t('desktop.nativeMenu.newThread', 'New Thread'),
    open_folder: t('desktop.nativeMenu.openFolder', 'Open Folder…'),
    close_window: t('desktop.nativeMenu.closeWindow', 'Close Window'),
    settings: t('desktop.nativeMenu.settings', 'Settings…'),
    edit: t('desktop.nativeMenu.edit', 'Edit'),
    undo: t('desktop.nativeMenu.undo', 'Undo'),
    redo: t('desktop.nativeMenu.redo', 'Redo'),
    cut: t('desktop.nativeMenu.cut', 'Cut'),
    copy: t('desktop.nativeMenu.copy', 'Copy'),
    paste: t('desktop.nativeMenu.paste', 'Paste'),
    select_all: t('desktop.nativeMenu.selectAll', 'Select All'),
    view: t('desktop.nativeMenu.view', 'View'),
    command_palette: t('desktop.nativeMenu.commandPalette', 'Command Palette…'),
    toggle_sidebar: t('desktop.nativeMenu.toggleSidebar', 'Toggle Sidebar'),
    toggle_changes: t('desktop.nativeMenu.toggleChanges', 'Toggle Inspector'),
    back: t('desktop.nativeMenu.back', 'Back'),
    forward: t('desktop.nativeMenu.forward', 'Forward'),
    reload: t('desktop.nativeMenu.reload', 'Reload'),
    zoom_reset: t('desktop.nativeMenu.zoomReset', 'Actual Size'),
    zoom_in: t('desktop.nativeMenu.zoomIn', 'Zoom In'),
    zoom_out: t('desktop.nativeMenu.zoomOut', 'Zoom Out'),
    fullscreen: t('desktop.nativeMenu.fullscreen', 'Toggle Full Screen'),
    thread: t('desktop.nativeMenu.thread', 'Thread'),
    send_to_cloud: t('desktop.nativeMenu.sendToCloud', 'Send to Cloud…'),
    open_in_chat: t('desktop.nativeMenu.openInChat', 'Open in Chat'),
    toggle_plan: t('desktop.nativeMenu.togglePlan', 'Plan Mode'),
    choose_agent: t('desktop.nativeMenu.chooseAgent', 'Choose Agent…'),
    folder: t('desktop.nativeMenu.folder', 'Folder'),
    index_settings: t('desktop.nativeMenu.indexSettings', 'Index Settings…'),
    handoff_settings: t('desktop.nativeMenu.handoffSettings', 'Handoff Settings…'),
    window: t('desktop.nativeMenu.window', 'Window'),
    minimize: t('desktop.nativeMenu.minimize', 'Minimize'),
    maximize: t('desktop.nativeMenu.maximize', 'Zoom'),
    help_menu: t('desktop.nativeMenu.helpMenu', 'Help'),
    help: t('desktop.nativeMenu.help', 'Elitea Help'),
    run_diagnostics: t('desktop.nativeMenu.runDiagnostics', 'Run Diagnostics…'),
    tray_show: t('desktop.nativeMenu.trayShow', 'Show Elitea'),
    tray_quit: t('desktop.nativeMenu.trayQuit', 'Quit Elitea'),
    activity_nothing: t('desktop.nativeMenu.activityNothing', 'Nothing running'),
    activity_running: t('desktop.nativeMenu.activityRunning', 'Running'),
    activity_approve: t('desktop.nativeMenu.activityApprove', 'Approve {tool}'),
    activity_approve_cloud: t('desktop.nativeMenu.activityApproveCloud', 'Approve in cloud'),
    activity_running_cloud: t('desktop.nativeMenu.activityRunningCloud', 'Running in cloud'),
  };
}
