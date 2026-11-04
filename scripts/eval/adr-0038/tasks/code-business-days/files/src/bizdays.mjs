// Business-day arithmetic (UTC calendar days, weekends + holidays excluded).

/**
 * @param {Date} date
 * @param {{ holidays?: string[] }} [options]
 * @returns {boolean}
 */
export function isBusinessDay(date, { holidays = [] } = {}) {
  throw new Error('not implemented');
}

/**
 * @param {Date} date
 * @param {number} n
 * @param {{ holidays?: string[] }} [options]
 * @returns {Date}
 */
export function addBusinessDays(date, n, { holidays = [] } = {}) {
  throw new Error('not implemented');
}

/**
 * @param {Date} start
 * @param {Date} end
 * @param {{ holidays?: string[] }} [options]
 * @returns {number}
 */
export function businessDaysBetween(start, end, { holidays = [] } = {}) {
  throw new Error('not implemented');
}
