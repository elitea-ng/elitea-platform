/**
 * The web shell's right side panel (`widgets/app-shell/ui/WebSidePanel.tsx`)
 * from a journey: the chat's participants live in its "Conversation" tab, a
 * list's tags and type filters in its "Filters" tab. The panel opens by
 * default on a wide window, but its open state is remembered per browser, so
 * a journey never assumes it — it asks for the tab and the panel opens if it
 * has to.
 */
import { expect, type Page } from '@playwright/test';

/** Shows the side panel with tab `tab` selected (`conversation`, `filters`). */
export async function openSidePanelTab(page: Page, tab: string): Promise<void> {
  const chip = page.getByTestId(`side-panel-chip-${tab}`);
  if (!(await chip.isVisible())) {
    const toggle = page.getByTestId('shell-side-panel-toggle');
    await expect(toggle).toBeVisible({ timeout: 15_000 });
    if ((await toggle.getAttribute('aria-pressed')) !== 'true') await toggle.click();
  }
  await expect(chip).toBeVisible({ timeout: 15_000 });
  if ((await chip.getAttribute('aria-selected')) !== 'true') await chip.click();
}

/** Hides the side panel (the title row's toggle) when it is shown. */
export async function closeSidePanel(page: Page): Promise<void> {
  const toggle = page.getByTestId('shell-side-panel-toggle');
  if ((await toggle.count()) > 0 && (await toggle.getAttribute('aria-pressed')) === 'true') await toggle.click();
  await expect(page.getByTestId('web-side-panel')).toHaveCount(0);
}

/** Opens the chat's participants: the side panel's Conversation tab. */
export async function openParticipants(page: Page): Promise<void> {
  await openSidePanelTab(page, 'conversation');
  await expect(page.getByTestId('participants-container')).toBeVisible({ timeout: 15_000 });
}
