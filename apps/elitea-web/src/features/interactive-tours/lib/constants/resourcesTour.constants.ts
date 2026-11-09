/**
 * Port of `apps/elitea-ui/src/[fsd]/features/interactive-tours/lib/constants/resourcesTour.constants.js`
 */

import type { TourStep } from '../types';

import { RESOURCES_TOUR_TARGETS } from './resourcesTourTargets.constants';
import { productName } from '@/shared/brand';

export const RESOURCES_TOUR_ID = 'resources';

export const RESOURCES_TOUR_COMPLETION = {
  keepExploring: [] as Array<{ label: string; tourId: string; path?: string }>,
};

export const resourcesTourSteps: TourStep[] = [
  {
    id: 'what-is-resources',
    target: RESOURCES_TOUR_TARGETS.page,
    placement: 'center',
    title: `What is ${productName()} Help Center?`,
    content: `The Help Center page is a central hub for learning and reference material. It provides quick access to documentation, release notes, video walkthroughs, tutorials, and interactive tours — all in one place, without leaving the platform.

The page also displays the current ${productName()} version and any installed plugin versions in the header area.`,
  },
  {
    id: 'documentation',
    target: RESOURCES_TOUR_TARGETS.documentationCard,
    placement: 'right',
    title: 'Documentation',
    content: `The Documentation card links to API references, feature guides, and platform concept documentation. Use it to look up specific capabilities, understand how features work, or integrate ${productName()} with external tools and SDKs.

Each link opens in a new tab. Links are configured by your platform administrator.`,
  },
  {
    id: 'release-notes',
    target: RESOURCES_TOUR_TARGETS.releaseNotesCard,
    placement: 'left',
    title: 'Release Notes',
    content: `The Release Notes card provides direct links to product update summaries — including new features, improvements, and bug fixes for each release. Use it to stay informed about what has changed between versions.

Links are configured by your platform administrator.`,
  },
  {
    id: 'video-library',
    target: RESOURCES_TOUR_TARGETS.videoLibraryCard,
    placement: 'right',
    title: 'Video Library',
    content: `The Video Library card links to recorded product walkthroughs and session recordings. Use it to watch feature demonstrations, onboarding walkthroughs, or recorded training sessions at your own pace.

Links are configured by your platform administrator.`,
  },
  {
    id: 'tutorials',
    target: RESOURCES_TOUR_TARGETS.tutorialsCard,
    placement: 'left',
    title: 'Tutorials',
    content: `The Tutorials card links to step-by-step guides and use-case walkthroughs. Use it to follow along with practical examples that show how to build agents, configure pipelines, set up toolkits, and accomplish common tasks.

Links are configured by your platform administrator.`,
  },
  {
    id: 'interactive-tours',
    target: RESOURCES_TOUR_TARGETS.interactiveToursCard,
    placement: 'right',
    title: 'Interactive Tours',
    content: `The Interactive Tours card links to guided, in-product tours that walk you through key features and workflows one step at a time. Use them to explore unfamiliar areas of the platform with contextual guidance at each step.

Links are configured by your platform administrator.`,
  },
];
