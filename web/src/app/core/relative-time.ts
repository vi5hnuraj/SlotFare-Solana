/**
 * The single place times are formatted for display: relative readings
 * (`relativeTime`, `maturityHint`), absolute ones (`localTime`) and
 * durations (`humanDuration`).
 * Templates call these; they never do ad-hoc arithmetic or slicing.
 * The clock is injected where it matters so the helpers are deterministic
 * in tests and reusable for any "now". The wire stays UTC; only display
 * converts, and absolute times always read in the viewer's local zone.
 */
const SECOND_MS = 1_000;
const MINUTE_MS = 60_000;
const HOUR_MS = 3_600_000;
const DAY_MS = 86_400_000;

/**
 * A human relative time for a timestamp. `null` or an unparseable value is
 * "Never" (a key that has not been used yet). The elapsed time is floored
 * into its bucket, so 90 s reads as "1 min ago".
 */
export function relativeTime(ts: string | null, now: number = Date.now()): string {
  if (ts === null) return 'Never';
  const t = Date.parse(ts);
  if (Number.isNaN(t)) return 'Never';

  const elapsed = now - t;
  if (elapsed < SECOND_MS) return 'just now';
  if (elapsed < MINUTE_MS) return `${Math.floor(elapsed / SECOND_MS)}s ago`;
  if (elapsed < HOUR_MS) return `${Math.floor(elapsed / MINUTE_MS)} min ago`;
  if (elapsed < DAY_MS) return `${Math.floor(elapsed / HOUR_MS)} h ago`;
  return `${Math.floor(elapsed / DAY_MS)} d ago`;
}

/**
 * "Matures in …" for a not-yet-matured listing, from its unix-seconds
 * `maturesAt`. A past or current time reads "Matures now" (display only;
 * the server's `matured` boolean still decides buyability). The gap is
 * floored into the same buckets as `relativeTime`.
 */
export function maturityHint(sec: number, now: number = Date.now()): string {
  const gap = sec * SECOND_MS - now;
  if (gap <= 0) return 'Matures now';
  if (gap < MINUTE_MS) return 'Matures in less than a minute';
  if (gap < HOUR_MS) return `Matures in ${Math.floor(gap / MINUTE_MS)} min`;
  if (gap < DAY_MS) return `Matures in ${Math.floor(gap / HOUR_MS)} h`;
  return `Matures in ${Math.floor(gap / DAY_MS)} d`;
}

/**
 * An absolute timestamp rendered in the viewer's local zone, formatted per
 * the browser locale, with no timezone suffix of any kind. `null` or an
 * empty value reads as "-" (the tables' dash convention); an unparseable
 * value passes through unchanged so no cell can ever read "Invalid Date".
 */
export function localTime(ts: string | null): string {
  if (ts === null || ts === '') return '-';
  const t = Date.parse(ts);
  if (Number.isNaN(t)) return ts;
  return new Date(t).toLocaleString();
}

/**
 * A wire duration in milliseconds, humanized: under a second reads as
 * "541 ms", from a second up as seconds with one decimal, trailing ".0"
 * dropped ("1 s", "12.7 s"). Zero is a measurement and reads "0 ms"; a
 * `null` duration is absence, not zero, and reads "-" (the tables' dash
 * convention) - this helper can never emit "0 ms" for a missing value.
 */
export function humanDuration(ms: number | null): string {
  if (ms === null) return '-';
  if (ms < SECOND_MS) return `${ms} ms`;
  return `${(ms / SECOND_MS).toFixed(1).replace(/\.0$/, '')} s`;
}
