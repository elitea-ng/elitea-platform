/**
 * Marketplace helpers.
 */
import { OTHER_CATEGORY } from './constants';
import type { ApplicationData } from './types';

/**
 * The category an agent is filed under: `meta.category`, the field the real
 * bulk-list endpoint populates (it comes straight from the version's `meta`
 * jsonb). `category` is a flat fallback for a caller that already has one.
 */
export const getCategoryForApplication = (app: ApplicationData): string => {
  return app.meta?.category || app.category || OTHER_CATEGORY;
};
