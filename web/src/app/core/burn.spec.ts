import { UsageRow } from './models';
import {
  bucketDaily,
  burnPerDay,
  creditsPerHour,
  dayZeroFor,
  occurrence,
  periodStats,
  runwayDays,
  spendLast24h,
} from './burn';

/** A fixture week, hand-computed against a fixed "now". */
const NOW = Date.UTC(2026, 7, 22, 20); // 2026-08-22T20:00:00Z

function row(time: string, credits: number): UsageRow {
  return {
    id: time,
    time,
    key: 'k',
    model: 'm',
    prompt: null,
    cached: null,
    completion: null,
    credits,
    result: '200',
    latencyMs: null,
    error: null,
    commitment: null,
  };
}

describe('occurrence', () => {
  it('parses an ISO timestamp to epoch millis', () => {
    expect(occurrence('2026-08-22T19:58:00Z', NOW)).toBe(
      Date.parse('2026-08-22T19:58:00Z'),
    );
  });

  it('returns null for a time-of-day string the mock log uses', () => {
    expect(occurrence('14:22:07', NOW)).toBeNull();
  });
});

describe('spendLast24h and creditsPerHour', () => {
  // Two rows inside the last 24 h (19:58 and 16:00 on the 22nd), one older.
  const rows = [
    row('2026-08-22T19:58:00Z', 100),
    row('2026-08-22T16:00:00Z', 50),
    row('2026-08-21T09:00:00Z', 999),
  ];

  it('sums only the rows within the last 24 hours', () => {
    expect(spendLast24h(rows, NOW)).toBe(150);
  });

  it('divides that spend by 24 for credits per hour', () => {
    expect(creditsPerHour(rows, NOW)).toBeCloseTo(150 / 24, 6);
  });

  it('counts unparseable (time-of-day) rows as now', () => {
    const hhmmss = [row('14:22:07', 30)];
    expect(spendLast24h(hhmmss, NOW)).toBe(30);
  });

  it('is zero when no row falls in the window', () => {
    expect(spendLast24h([row('2026-08-01T09:00:00Z', 10)], NOW)).toBe(0);
  });
});

describe('burnPerDay over a fixture week', () => {
  it('averages seven days of usage by dividing by seven', () => {
    const week = [
      row('2026-08-22T01:00:00Z', 10),
      row('2026-08-21T01:00:00Z', 20),
      row('2026-08-20T01:00:00Z', 30),
      row('2026-08-19T01:00:00Z', 40),
      row('2026-08-18T01:00:00Z', 50),
      row('2026-08-17T01:00:00Z', 60),
      row('2026-08-16T01:00:00Z', 70),
    ];
    // Total 280 over 7 days -> 40 credits/day.
    expect(burnPerDay(week, NOW)).toBe(40);
  });

  it('excludes rows outside the seven-day window', () => {
    const week = [
      row('2026-08-22T01:00:00Z', 10),
      row('2026-08-15T01:00:00Z', 1_000), // eight days old, out of window
    ];
    expect(burnPerDay(week, NOW)).toBe(10 / 7);
  });

  it('is null for an account with no usage in the window', () => {
    expect(burnPerDay([], NOW)).toBeNull();
    expect(burnPerDay([row('2026-08-01T09:00:00Z', 10)], NOW)).toBeNull();
  });
});

describe('runwayDays', () => {
  it('divides balance by burn rate', () => {
    expect(runwayDays(400, 40)).toBe(10);
  });

  it('is null when the burn rate is undefined', () => {
    expect(runwayDays(100, null)).toBeNull();
  });

  it('is null when the burn rate is zero', () => {
    expect(runwayDays(100, 0)).toBeNull();
  });

  it('never returns Infinity or NaN', () => {
    expect(Number.isFinite(runwayDays(100, null) ?? 0)).toBe(true);
    expect(runwayDays(0, 40)).toBe(0);
  });
});

describe('dayZeroFor', () => {
  it('is the UTC midnight of the first day of the window', () => {
    // NOW is 2026-08-22T20:00Z; a 3-day window starts on 2026-08-20.
    expect(dayZeroFor(3, NOW)).toBe(Date.UTC(2026, 7, 20));
  });

  it('floors to UTC midnight even when now is not at midnight', () => {
    // 2026-08-22T00:00:00Z exactly: a 1-day window starts that same day.
    expect(dayZeroFor(1, Date.UTC(2026, 7, 22))).toBe(Date.UTC(2026, 7, 22));
    // A second before midnight still belongs to the previous day's window.
    expect(dayZeroFor(1, Date.UTC(2026, 7, 22) - 1_000)).toBe(
      Date.UTC(2026, 7, 21),
    );
  });
});

describe('bucketDaily', () => {
  it('lands a row on each of two days in the right buckets', () => {
    const rows = [
      row('2026-08-21T09:00:00Z', 10),
      row('2026-08-22T19:58:00Z', 5),
    ];
    // Window 2026-08-20..22 (3 days): index 1 is the 21st, index 2 the 22nd.
    expect(bucketDaily(rows, 3, NOW, (r) => r.credits)).toEqual([0, 10, 5]);
  });

  it('is zero for a day with no rows and counts one per row for requests', () => {
    const rows = [row('2026-08-22T19:58:00Z', 100)];
    expect(bucketDaily(rows, 3, NOW, () => 1)).toEqual([0, 0, 1]);
  });

  it('excludes rows outside the window', () => {
    const rows = [
      row('2026-08-19T09:00:00Z', 999), // one day before the window
      row('2026-08-22T23:59:00Z', 1), // after `now` but same day
    ];
    expect(bucketDaily(rows, 3, NOW, (r) => r.credits)).toEqual([0, 0, 1]);
  });

  it('counts unparseable (time-of-day) rows as now', () => {
    const rows = [row('14:22:07', 30)];
    expect(bucketDaily(rows, 3, NOW, (r) => r.credits)).toEqual([0, 0, 30]);
  });

  it('skips rows whose pick returns null', () => {
    const rows = [row('2026-08-22T19:58:00Z', 30)];
    expect(bucketDaily(rows, 3, NOW, () => null)).toEqual([0, 0, 0]);
  });
});

describe('periodStats', () => {
  it('computes total, mean, peak value and peak index', () => {
    const s = [10, 30, 20, 40];
    expect(periodStats(s)).toEqual({
      total: 100,
      mean: 25,
      peakValue: 40,
      peakIndex: 3,
    });
  });

  it('reports the first maximum when values tie', () => {
    expect(periodStats([5, 9, 9, 1])?.peakIndex).toBe(1);
  });

  it('is null for an empty series', () => {
    expect(periodStats([])).toBeNull();
  });

  it('never yields NaN or Infinity for a non-empty series', () => {
    const p = periodStats([1]);
    expect(p).not.toBeNull();
    expect(Number.isFinite(p!.total)).toBe(true);
    expect(Number.isFinite(p!.mean)).toBe(true);
  });
});
