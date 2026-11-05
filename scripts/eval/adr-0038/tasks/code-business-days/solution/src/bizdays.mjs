// Business-day arithmetic (UTC calendar days, weekends + holidays excluded).

const DAY_MS = 86_400_000;
const ISO_DAY = /^\d{4}-\d{2}-\d{2}$/;

function holidaySet(holidays) {
  for (const h of holidays) {
    if (typeof h !== 'string' || !ISO_DAY.test(h) || Number.isNaN(Date.parse(`${h}T00:00:00Z`))) {
      throw new RangeError(`invalid holiday: ${h}`);
    }
  }
  return new Set(holidays);
}

const dayKey = (date) => date.toISOString().slice(0, 10);

function isBusiness(date, set) {
  const dow = date.getUTCDay();
  return dow !== 0 && dow !== 6 && !set.has(dayKey(date));
}

/**
 * @param {Date} date
 * @param {{ holidays?: string[] }} [options]
 * @returns {boolean}
 */
export function isBusinessDay(date, { holidays = [] } = {}) {
  return isBusiness(date, holidaySet(holidays));
}

/**
 * @param {Date} date
 * @param {number} n
 * @param {{ holidays?: string[] }} [options]
 * @returns {Date}
 */
export function addBusinessDays(date, n, { holidays = [] } = {}) {
  if (!Number.isInteger(n)) throw new RangeError(`n must be an integer, got ${n}`);
  const set = holidaySet(holidays);
  const cur = new Date(date.getTime());
  if (n === 0) {
    while (!isBusiness(cur, set)) cur.setUTCDate(cur.getUTCDate() + 1);
    return cur;
  }
  const step = n > 0 ? 1 : -1;
  let remaining = Math.abs(n);
  while (remaining > 0) {
    cur.setUTCDate(cur.getUTCDate() + step);
    if (isBusiness(cur, set)) remaining--;
  }
  return cur;
}

const utcDayNumber = (date) =>
  Math.floor(Date.UTC(date.getUTCFullYear(), date.getUTCMonth(), date.getUTCDate()) / DAY_MS);

/**
 * @param {Date} start
 * @param {Date} end
 * @param {{ holidays?: string[] }} [options]
 * @returns {number}
 */
export function businessDaysBetween(start, end, { holidays = [] } = {}) {
  const set = holidaySet(holidays);
  const a = utcDayNumber(start);
  const b = utcDayNumber(end);
  if (a === b) return 0;
  const [lo, hi, sign] = a < b ? [a, b, 1] : [b, a, -1];
  let count = 0;
  for (let d = lo + 1; d <= hi; d++) {
    if (isBusiness(new Date(d * DAY_MS), set)) count++;
  }
  return sign * count;
}
