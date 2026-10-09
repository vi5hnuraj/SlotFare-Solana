import { AttestChain } from './api';
import {
  ATTEST_LAGGING_BLOCKS,
  ATTEST_LAGGING_SEC,
  ATTEST_NORMAL_BLOCKS,
  ATTEST_NORMAL_SEC,
  attestGrade,
  attestLagText,
  attestRows,
  attestTipText,
  gradePillClass,
} from './attestation-health';

/** One row with sensible defaults; tests override what they exercise. */
function row(overrides: Partial<AttestChain> = {}): AttestChain {
  return {
    chainKey: 1,
    name: 'Ethereum',
    known: true,
    attestedTip: 1_000_000,
    head: 1_000_001,
    lagBlocks: 0,
    lagSeconds: 0,
    ...overrides,
  };
}

describe('attestLagText', () => {
  it('phrases a seconds lag as "<N> s behind"', () => {
    expect(attestLagText(row({ lagSeconds: 38 }))).toBe('38 s behind');
  });

  it('phrases a block lag when the seconds are undetermined', () => {
    expect(attestLagText(row({ lagBlocks: 12 }))).toBe('12 blocks behind');
  });

  it('prefers the seconds over the blocks when both are present', () => {
    expect(attestLagText(row({ lagBlocks: 12, lagSeconds: 38 }))).toBe(
      '38 s behind',
    );
  });

  it('reads a known zero-lag row as up to date', () => {
    expect(attestLagText(row())).toBe('up to date');
  });

  it('reads an unknown row as unreachable', () => {
    expect(attestLagText(row({ known: false }))).toBe('unreachable');
  });
});

describe('attestTipText', () => {
  it('renders a known positive tip as a grouped number', () => {
    expect(attestTipText(row({ attestedTip: 1_000_000 }))).toBe('1,000,000');
  });

  it('reads a known zero tip as none (nothing attested yet)', () => {
    expect(attestTipText(row({ attestedTip: 0 }))).toBe('none');
  });

  it('reads an unknown tip as a dash', () => {
    expect(attestTipText(row({ known: false, attestedTip: 0 }))).toBe('-');
  });
});

describe('attestGrade', () => {
  it('grades at the seconds boundaries', () => {
    expect(attestGrade(row({ lagSeconds: ATTEST_NORMAL_SEC }))).toBe('normal');
    expect(attestGrade(row({ lagSeconds: ATTEST_NORMAL_SEC + 1 }))).toBe('lagging');
    expect(attestGrade(row({ lagSeconds: ATTEST_LAGGING_SEC }))).toBe('lagging');
    expect(attestGrade(row({ lagSeconds: ATTEST_LAGGING_SEC + 1 }))).toBe('stalled');
  });

  it('falls back to the block boundaries when the seconds are undetermined', () => {
    expect(attestGrade(row({ lagBlocks: ATTEST_NORMAL_BLOCKS }))).toBe('normal');
    expect(attestGrade(row({ lagBlocks: ATTEST_NORMAL_BLOCKS + 1 }))).toBe('lagging');
    expect(attestGrade(row({ lagBlocks: ATTEST_LAGGING_BLOCKS }))).toBe('lagging');
    expect(attestGrade(row({ lagBlocks: ATTEST_LAGGING_BLOCKS + 1 }))).toBe('stalled');
  });

  it('lets the seconds win over the blocks when both are present', () => {
    // A small block lag with a large seconds lag is still stalled.
    expect(attestGrade(row({ lagBlocks: 1, lagSeconds: ATTEST_LAGGING_SEC + 1 }))).toBe('stalled');
    // A large block lag with a small seconds lag stays normal.
    expect(attestGrade(row({ lagBlocks: ATTEST_LAGGING_BLOCKS + 1, lagSeconds: ATTEST_NORMAL_SEC }))).toBe('normal');
  });

  it('grades an unknown row as unreachable regardless of the lags', () => {
    expect(attestGrade(row({ known: false, lagBlocks: 99, lagSeconds: 999 }))).toBe('unreachable');
  });
});

describe('gradePillClass', () => {
  it('maps each grade onto its design-system pill class', () => {
    expect(gradePillClass('normal')).toBe('pill-ok');
    expect(gradePillClass('lagging')).toBe('pill-live');
    expect(gradePillClass('stalled')).toBe('pill-alert');
    expect(gradePillClass('unreachable')).toBe('pill-wait');
  });
});

describe('attestRows', () => {
  it('maps one display row per chain in wire order', () => {
    const rows = attestRows([
      row({ chainKey: 1, name: 'Ethereum', lagSeconds: 12 }),
      row({ chainKey: 3, name: 'Ethereum', known: false }),
    ]);
    expect(rows.map((r) => r.chainKey)).toEqual([1, 3]);
    expect(rows[0]).toEqual({
      chainKey: 1,
      name: 'Ethereum',
      known: true,
      tipText: '1,000,000',
      lagText: '12 s behind',
      grade: 'normal',
      pillClass: 'pill-ok',
    });
    expect(rows[1]).toEqual({
      chainKey: 3,
      name: 'Ethereum',
      known: false,
      tipText: '-',
      lagText: 'unreachable',
      grade: 'unreachable',
      pillClass: 'pill-wait',
    });
  });
});
