import { humanDuration, localTime, relativeTime } from './relative-time';

const NOW = Date.UTC(2026, 7, 22, 20); // 2026-08-22T20:00:00Z

describe('relativeTime', () => {
  it('reads sub-second elapsed as "just now"', () => {
    expect(relativeTime('2026-08-22T19:59:59.500Z', NOW)).toBe('just now');
  });

  it('floors seconds into "Ns ago"', () => {
    expect(relativeTime('2026-08-22T19:59:15Z', NOW)).toBe('45s ago');
  });

  it('floors minutes into "N min ago"', () => {
    expect(relativeTime('2026-08-22T19:58:00Z', NOW)).toBe('2 min ago');
  });

  it('floors hours into "N h ago"', () => {
    expect(relativeTime('2026-08-22T16:00:00Z', NOW)).toBe('4 h ago');
  });

  it('floors days into "N d ago"', () => {
    expect(relativeTime('2026-08-19T10:00:00Z', NOW)).toBe('3 d ago');
  });

  it('returns "Never" for a null timestamp', () => {
    expect(relativeTime(null, NOW)).toBe('Never');
  });

  it('returns "Never" for an unparseable timestamp', () => {
    expect(relativeTime('not a date', NOW)).toBe('Never');
  });

  it('uses the injected clock, not the wall clock', () => {
    const ts = '2026-08-22T19:58:00Z';
    // Two different clocks give two different readings of the same moment.
    expect(relativeTime(ts, NOW)).toBe('2 min ago');
    expect(relativeTime(ts, NOW + 2 * 3_600_000)).toBe('2 h ago');
  });
});

describe('localTime', () => {
  it('renders a known epoch in the runner\'s local zone', () => {
    expect(localTime('2026-08-25T20:51:00Z')).toBe(
      new Date(Date.parse('2026-08-25T20:51:00Z')).toLocaleString(),
    );
  });

  it('carries no UTC suffix', () => {
    expect(localTime('2026-08-25T20:51:00Z')).not.toContain('UTC');
  });

  it('reads "-" for a null or empty timestamp', () => {
    expect(localTime(null)).toBe('-');
    expect(localTime('')).toBe('-');
  });

  it('passes an unparseable value through unchanged, never NaN', () => {
    expect(localTime('not a date')).toBe('not a date');
  });
});

describe('humanDuration', () => {
  it('reads sub-second durations as whole milliseconds', () => {
    expect(humanDuration(541)).toBe('541 ms');
    expect(humanDuration(999)).toBe('999 ms');
  });

  it('reads whole seconds without a trailing .0', () => {
    expect(humanDuration(1000)).toBe('1 s');
  });

  it('reads longer durations as seconds with one decimal', () => {
    expect(humanDuration(12700)).toBe('12.7 s');
  });

  it('keeps zero as a measurement', () => {
    expect(humanDuration(0)).toBe('0 ms');
  });

  it('reads a missing duration as a dash, never "0 ms"', () => {
    expect(humanDuration(null)).toBe('-');
  });
});
