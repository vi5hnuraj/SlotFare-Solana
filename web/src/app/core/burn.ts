import { UsageRow } from './models';

/**
 * Burn-rate and runway math, kept pure so it can be unit-tested against
 * hand-computed values without touching Angular or the clock. Every figure
 * on the overview and usage screens that claims to be an average or a
 * projection comes from these functions; nothing is hardcoded in a template.
 */

export const HOUR_MS = 3_600_000;
export const DAY_MS = 86_400_000;
export const WEEK_DAYS = 7;

/**
 * The epoch millis of a usage row's timestamp, or null when it cannot be
 * parsed. The mock log rows carry time-of-day strings (`HH:MM:SS`), which
 * are unparseable as absolute timestamps; callers treat a null as "now",
 * because those rows are the most recent ones in the store's ordering.
 */
export function occurrence(time: string, now: number): number | null {
  const t = Date.parse(time);
  return Number.isNaN(t) ? null : t;
}

/** True when the row's occurrence falls within `(now - windowMs, now]`. */
function inWindow(occ: number | null, now: number, windowMs: number): boolean {
  const t = occ ?? now;
  return t > now - windowMs && t <= now;
}

/** Credits spent across rows whose occurrence is within the last 24 h. */
export function spendLast24h(rows: UsageRow[], now: number): number {
  return rows.reduce((acc, r) => {
    if (!inWindow(occurrence(r.time, now), now, DAY_MS)) return acc;
    return acc + r.credits;
  }, 0);
}

/** Credits per hour over the last 24 h (spend divided by 24). */
export function creditsPerHour(rows: UsageRow[], now: number): number {
  return spendLast24h(rows, now) / 24;
}

/**
 * The seven-day mean burn rate, in credits per day: total credits over the
 * last seven days divided by seven. Null when no row falls in the window.
 * A brand-new account has no usage, so its burn rate is undefined rather
 * than zero or infinity.
 */
export function burnPerDay(rows: UsageRow[], now: number): number | null {
  let total = 0;
  let any = false;
  for (const r of rows) {
    if (inWindow(occurrence(r.time, now), now, WEEK_DAYS * DAY_MS)) {
      total += r.credits;
      any = true;
    }
  }
  return any ? total / WEEK_DAYS : null;
}

/**
 * How many days the balance lasts at a given burn rate. Null when the burn
 * rate is null or non-positive: undefined, not Infinity, not zero.
 */
export function runwayDays(balance: number, burn: number | null): number | null {
  if (burn === null || burn <= 0) return null;
  return balance / burn;
}

/**
 * The UTC-midnight epoch millis of the first day of a `days`-day window
 * ending at `now` (i.e. `now - (days - 1) * DAY_MS`, floored to UTC
 * midnight). The index of a day in `bucketDaily`'s output is its distance
 * from this value, in whole days.
 */
export function dayZeroFor(days: number, now: number): number {
  const first = now - (days - 1) * DAY_MS;
  const d = new Date(first);
  return Date.UTC(d.getUTCFullYear(), d.getUTCMonth(), d.getUTCDate());
}

/**
 * Sum `pick(row)` per UTC calendar day across a `days`-day window ending at
 * `now`: index `i` covers the day `dayZeroFor(days, now) + i * DAY_MS`. A
 * null occurrence is treated as "now" (the mock's time-of-day rows are the
 * most recent); rows outside the window are dropped, and days with no rows
 * are 0. The single source for the usage screen's credits-per-day and
 * requests-per-day series, so the chart can only ever show rows that are in
 * the store.
 */
export function bucketDaily(
  rows: UsageRow[],
  days: number,
  now: number,
  pick: (r: UsageRow) => number | null,
): number[] {
  const zero = dayZeroFor(days, now);
  const out = new Array<number>(days).fill(0);
  for (const r of rows) {
    const t = occurrence(r.time, now) ?? now;
    const idx = Math.floor((t - zero) / DAY_MS);
    if (idx < 0 || idx >= days) continue;
    const v = pick(r);
    if (v !== null) out[idx] += v;
  }
  return out;
}

/** Total, mean, peak value and the index of that peak for one series. */
export interface PeriodStats {
  total: number;
  mean: number;
  peakValue: number;
  peakIndex: number;
}

/**
 * Aggregate stats over a per-day series (daily spend or daily requests).
 * Null for an empty series, so a screen with no data shows a dash instead
 * of a NaN.
 */
export function periodStats(series: number[]): PeriodStats | null {
  if (series.length === 0) return null;
  let total = 0;
  let peakValue = series[0];
  let peakIndex = 0;
  for (let i = 0; i < series.length; i++) {
    total += series[i];
    if (series[i] > peakValue) {
      peakValue = series[i];
      peakIndex = i;
    }
  }
  return { total, mean: total / series.length, peakValue, peakIndex };
}
