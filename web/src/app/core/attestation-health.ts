import { AttestChain } from './api';
import { num } from './pricing';

/**
 * The Attestation card's presentation layer: the human lag phrasing and the
 * color grade for each source chain. Pure functions over wire data, so the
 * card's markup stays a thin template.
 *
 * The thresholds below are display judgment, not protocol claims: they only
 * decide which pill color a lag gets, and they say nothing about whether a
 * proof is valid or a deposit is safe.
 */

/**
 * Lag grade thresholds. Seconds are preferred when the server reports them
 * (it could read both block timestamps); blocks are the fallback when the
 * seconds are undetermined. Rationale: 30 s is a few short-block times on a
 * healthy chain; 5 min is where the provers are clearly falling behind. The
 * block cutoffs are coarser, so they use rounder, larger numbers.
 */
export const ATTEST_NORMAL_SEC = 30;
export const ATTEST_LAGGING_SEC = 300;
export const ATTEST_NORMAL_BLOCKS = 3;
export const ATTEST_LAGGING_BLOCKS = 20;

export type AttestGrade = 'normal' | 'lagging' | 'stalled' | 'unreachable';

/** One rendered row of the card: the chain's identity plus its display text. */
export interface AttestRow {
  chainKey: number;
  name: string;
  known: boolean;
  tipText: string;
  lagText: string;
  grade: AttestGrade;
  pillClass: string;
}

/**
 * Grade one row. An unknown read is unreachable; otherwise seconds win when
 * present, and the block fallback covers the undetermined-seconds case.
 */
export function attestGrade(c: AttestChain): AttestGrade {
  if (!c.known) return 'unreachable';
  if (c.lagSeconds > 0) {
    return c.lagSeconds <= ATTEST_NORMAL_SEC
      ? 'normal'
      : c.lagSeconds <= ATTEST_LAGGING_SEC
        ? 'lagging'
        : 'stalled';
  }
  return c.lagBlocks <= ATTEST_NORMAL_BLOCKS
    ? 'normal'
    : c.lagBlocks <= ATTEST_LAGGING_BLOCKS
      ? 'lagging'
      : 'stalled';
}

/**
 * The lag phrased for humans: "38 s behind", "8 min behind" from 90 s up,
 * "12 blocks behind", "up to
 * date" - or "unreachable" when the server could not read the chain.
 */
export function attestLagText(c: AttestChain): string {
  if (!c.known) return 'unreachable';
  if (c.lagSeconds >= 90) return `${num(Math.round(c.lagSeconds / 60))} min behind`;
  if (c.lagSeconds > 0) return `${num(c.lagSeconds)} s behind`;
  if (c.lagBlocks > 0) return `${num(c.lagBlocks)} blocks behind`;
  return 'up to date';
}

/** The attested tip as display text: "-" when unknown, "none" when nothing
 *  has been attested yet. */
export function attestTipText(c: AttestChain): string {
  if (!c.known) return '-';
  return c.attestedTip > 0 ? num(c.attestedTip) : 'none';
}

/**
 * The design-system pill class per grade: teal for healthy, slate for
 * lagging, gold for stalled, neutral for unreachable. These are the
 * existing `.pill-*` classes from `src/styles/slotfare.scss`; no new CSS.
 */
export function gradePillClass(g: AttestGrade): string {
  switch (g) {
    case 'normal':
      return 'pill-ok';
    case 'lagging':
      return 'pill-live';
    case 'stalled':
      return 'pill-alert';
    default:
      return 'pill-wait';
  }
}

/** One `AttestRow` per source chain, in wire order. */
export function attestRows(chains: AttestChain[]): AttestRow[] {
  return chains.map((c) => {
    const grade = attestGrade(c);
    return {
      chainKey: c.chainKey,
      name: c.name,
      known: c.known,
      tipText: attestTipText(c),
      lagText: attestLagText(c),
      grade,
      pillClass: gradePillClass(grade),
    };
  });
}
