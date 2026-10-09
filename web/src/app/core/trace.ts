import { creditText, num, usd } from './pricing';
import { FundingStatus } from './api';
import { localTime } from './relative-time';
import { environment } from '../../environments/environment';
export type Lane = 'eth' | 'proof' | 'cc';

export const LANES: Record<Lane, { css: string; label: string }> = {
  eth: { css: 'lane-eth', label: 'Solana' },
  proof: { css: 'lane-proof', label: 'Attestation' },
  cc: { css: 'lane-cc', label: 'Slotfare' },
};

export interface MetaRow {
  label: string;
  value: string;
}

/** A progress readout under a step: either a bar, a row of confirmation
 *  ticks, or a settled value pill. */
export type Confirmation =
  | { kind: 'meter'; percent: number; text: string }
  | { kind: 'ticks'; on: number; total: number; text: string }
  | { kind: 'value'; text: string }
  | null;

export interface StepView {
  index: number;
  lane: Lane;
  laneLabel: string;
  laneCss: string;
  /** true when this step opens a new lane, so the template draws a header */
  laneStart: boolean;
  state: StepState;
  title: string;
  when: string;
  meta: MetaRow[];
  confirmation: Confirmation;
  /** Empty while the fact the link would point at is still unknown. */
  linkHref: string;
  linkLabel: string;
  /** 0..1, drives the ring around the step number */
  progress: number;
}

export type StepState = 'done' | 'live' | 'pending';

export const short = (hash: string): string => `${hash.slice(0, 10)}…${hash.slice(-6)}`;

/** Middle truncation capped at `max` total characters: values at or under
 *  the cap pass through unchanged, longer ones keep their head and tail
 *  with an ellipsis where the middle was dropped. */
export function mid(text: string, max: number): string {
  if (text.length <= max) return text;
  if (max < 3) return text.slice(0, max);
  const head = Math.ceil((max - 1) / 2);
  const tail = max - 1 - head;
  return `${text.slice(0, head)}…${text.slice(-tail)}`;
}

/** Strip a trailing /tx/ or /tx segment from an explorer base so a
 *  different resource path can be appended without a double slash.
 *  A base without the segment passes through unchanged. */
function stripTx(base: string): string {
  return base.replace(/\/tx\/?$/, '');
}

/** Build a block-explorer URL. Accepts a base that may or may not
 *  carry a /tx/ suffix. The block number is the raw integer, never
 *  locale-formatted. Empty base returns empty string. */
export function blockUrl(base: string, n: number): string {
  return base !== '' ? `${stripTx(base)}/block/${n}` : '';
}

/** Build a transaction-explorer URL. If the base already ends in
 *  /tx (or /tx/), the hash is appended directly; otherwise /tx/ is
 *  inserted. No double slashes. Empty hash returns empty string. */
export function explorerTxUrl(base: string, tx: string): string {
  if (base === '' || tx === '') return '';
  const trimmed = base.replace(/\/+$/, '');
  return trimmed.endsWith('/tx') ? `${trimmed}/${tx}` : `${trimmed}/tx/${tx}`;
}

/**
 * The explorer URL for a transaction hash on the Slotfare cluster. The
 * the contract every explorer link in the console shares.
 */
export function solTxUrl(tx: string): string {
  if (tx === '') return '';
  const cluster = environment.network === 'testnet' ? '?cluster=devnet' : '';
  return `https://explorer.solana.com/tx/${tx}${cluster}`;
}

/**
 * The Solana explorer URL for a settlement transaction: usage roots land as
 * memo transactions on the Slotfare cluster. The console's default deployment
 * tracks devnet (reported as `testnet` in the environment), so devnet is the
 * default cluster; mainnet stays unqualified. Empty when the hash is unknown.
 */
/** Which step is currently live (the first not-yet-done step). A direct
 *  purchase has two rungs: 0 while the mint is in flight, 1 once settled.
 *  A proven purchase has four: 0 until the deposit is seen on chain,
 *  1 until the watcher attests its height, 2 while the mint settles, and
 *  3 once settled. Only legacy EVM rows still take that path; on this
 *  deployment every purchase is direct. */
export function stageIndex(s: FundingStatus): number {
  if (isDirectPurchase(s)) {
    return s.state === 'settled' ? 1 : 0;
  }
  if (s.state === 'settled') return 3;
  if (s.ethBlock === null) return 0;
  if (s.attestedHeight === null || s.attestedHeight < s.ethBlock) return 1;
  return 2;
}

export function isSettled(s: FundingStatus): boolean {
  return s.state === 'settled';
}

/**
 * A direct purchase is credited in the very transaction that pays it: one
 * step, no separate proof leg.
 *
 * This deployment has one chain, and chain key 0 is it: a USDC transfer to
 * the treasury is seen by the deposit watcher, which mints in the same pass.
 * There is no second chain to wait for and no attestation in between, so
 * every purchase the backend writes is direct. Keying off the source block
 * would misread it - a Solana deposit carries a real slot, so a non-zero
 * value means "this deposit landed at slot N", not "this purchase is still
 * being proven".
 *
 * A purchase that names any other chain is a legacy source-chain row
 * (imported history, chain key 1); those keep their three-step proof
 * anatomy, because for them the deposit, the attestation and the mint really
 * were separate events.
 */
export function isDirectPurchase(s: FundingStatus): boolean {
  return s.chainKey === 0;
}

/**
 * The proven purchase's three-step anatomy, resolved against real
 * on-chain facts: the deposit seen on chain, its height attested by the
 * watcher, and the credits minted. Everything a template needs is computed
 * here so the markup stays declarative. `sourceExplorer` is the
 * block-explorer base for the purchase's chain, read from `/console/chains`;
 * when it is empty the lane links are left blank rather than pointing at a
 * hardcoded host.
 */
export function buildSteps(s: FundingStatus, sourceExplorer = ''): StepView[] {
  // A direct purchase has exactly one stage that happened: the mint.
  // Drawing it as a multi-stage journey would show steps that never occurred.
  if (isDirectPurchase(s)) return directSteps(s);
  return provenSteps(s, sourceExplorer);
}

/** The three steps of a proven purchase, one per lane. States follow the
 *  existing done/live/pending rule over the wire facts: the deposit is seen
 *  once the source block is known, the height is attested once the provers
 *  reach it, and the credits are minted once the purchase settles. */
function provenSteps(s: FundingStatus, sourceExplorer: string): StepView[] {
  const lanes: Lane[] = ['eth', 'proof', 'cc'];
  const done: boolean[] = [
    s.ethBlock !== null,
    s.ethBlock !== null && s.attestedHeight !== null && s.attestedHeight >= s.ethBlock,
    s.state === 'settled',
  ];

  const views: StepView[] = [];
  for (let i = 0; i < 3; i++) {
    const state: StepState = done[i]
      ? 'done'
      : i === 0 || done[i - 1]
        ? 'live'
        : 'pending';
    const lane = lanes[i];
    views.push({
      index: i,
      lane,
      laneLabel: LANES[lane].label,
      laneCss: LANES[lane].css,
      laneStart: true,
      state,
      progress: state === 'done' ? 1 : state === 'live' ? 1 : 0,
      ...detail(i, state, s, sourceExplorer),
    });
  }
  return views;
}

/** The single step of a direct purchase: the deposit and the credits it
 *  minted, with the paying transaction, its slot and the credited amount.
 *  The explorer link follows the same rule as the mint step of the proven
 *  trace.
 *
 * The step is only done once the credits landed. A just-sent deposit is the
 * same single step, live, pointing at the transfer the wallet signed - it
 * must not claim "credits minted" before the watcher has seen it. */
function directSteps(s: FundingStatus): StepView[] {
  const settled = isSettled(s);
  const ccTx = settled ? s.ccTx ?? '' : '';
  const tx = ccTx !== '' ? ccTx : s.ethTx;
  const lane = LANES.cc;
  return [
    {
      index: 0,
      lane: 'cc',
      laneLabel: lane.label,
      laneCss: lane.css,
      laneStart: true,
      state: settled ? 'done' : 'live',
      title: settled ? 'Credits minted' : 'Deposit sent',
      when: settled ? localTime(s.updatedAt) : '',
      meta: [
        { label: 'Transaction', value: tx !== '' ? short(tx) : '-' },
        { label: 'Slot', value: settled && s.ccBlock !== null ? num(s.ccBlock) : '-' },
        ...(s.credits !== null ? [{ label: 'Credited', value: `${creditText(s.credits)} credits` }] : []),
      ],
      confirmation: settled
        ? {
            kind: 'value',
            text: `+${creditText(s.credits ?? 0)} credits · ${usd(s.credits ?? 0)}`,
          }
        : { kind: 'meter', percent: 50, text: 'Minting credits' },
      linkHref: solTxUrl(tx),
      linkLabel: 'Solana explorer · transaction',
      progress: 1,
    },
  ];
}

function detail(
  i: number,
  state: StepState,
  s: FundingStatus,
  sourceExplorer = '',
): Pick<StepView, 'title' | 'when' | 'meta' | 'confirmation' | 'linkHref' | 'linkLabel'> {
  switch (i) {
    case 0: {
      const seen = s.ethBlock !== null;
      return {
        title: seen
          ? `Deposit seen on ${s.chainName || 'Solana'} slot ${num(s.ethBlock!)}`
          : `Deposit seen on ${s.chainName || 'Solana'}`,
        when: state === 'done' ? localTime(s.startedAt) : '',
        meta: [
          { label: 'Transaction', value: s.ethTx !== '' ? short(s.ethTx) : '-' },
          { label: 'Network', value: s.chainName || '-' },
          { label: 'Slot', value: seen ? num(s.ethBlock!) : '-' },
        ],
        confirmation:
          state === 'live'
            ? {
                kind: 'meter',
                percent: 50,
                text: 'Awaiting confirmation',
              }
            : null,
        // A link needs a fact behind it: the block once it is known, the
        // pending transaction before that.
        linkHref: seen
          ? blockUrl(sourceExplorer, s.ethBlock!)
          : explorerTxUrl(sourceExplorer, s.ethTx),
        linkLabel: seen ? `Explorer · block ${num(s.ethBlock!)}` : 'Explorer · transaction',
      };
    }

    case 1: {
      const tip = s.chainTip ?? 0;
      return {
        title:
          s.attestedHeight !== null
            ? `Height attested by the watcher (${num(s.attestedHeight)})`
            : 'Height attested by the watcher',
        // The wire carries no attestation timestamp, so this step shows
        // none rather than inventing one.
        when: '',
        meta: [
          { label: 'Attested height', value: s.attestedHeight !== null ? num(s.attestedHeight) : '-' },
          { label: 'Chain tip', value: tip > 0 ? num(tip) : '-' },
        ],
        confirmation:
          state === 'pending'
            ? null
            : {
                kind: 'meter',
                percent: state === 'done' ? 100 : 60,
                text:
                  state === 'done'
                    ? 'Attestation quorum reached'
                    : `Attesting · chain tip ${tip > 0 ? num(tip) : '-'}`,
              },
        // The attested height is a height on the deposit chain; there is
        // no separate proof-layer dashboard to link it to, so the link
        // stays empty rather than pointing at a host nobody operates.
        linkHref: '',
        linkLabel: '',
      };
    }

    default: {
      const ccTx = s.ccTx ?? '';
      return {
        title: 'Deposit confirmed · credits minted',
        when: state === 'done' ? localTime(s.updatedAt) : '',
        meta:
          state === 'pending'
            ? [{ label: 'Waiting on', value: 'attestation quorum' }]
            : [
                { label: 'Transaction', value: ccTx !== '' ? short(ccTx) : '-' },
                { label: 'Block', value: s.ccBlock !== null ? num(s.ccBlock) : '-' },
                ...(s.credits !== null ? [{ label: 'Credited', value: `${creditText(s.credits)} credits` }] : []),
              ],
        confirmation:
          state === 'pending'
            ? null
            : state === 'live'
              ? {
                  kind: 'meter',
                  percent: 50,
                  text: 'Mint submitted · awaiting confirmation',
                }
              : {
                  kind: 'value',
                  text: s.credits !== null
                    ? `+${creditText(s.credits)} credits · ${usd(s.credits)}`
                    : 'Settled',
                },
        linkHref: ccTx !== '' ? solTxUrl(ccTx) : '',
        linkLabel: 'Solana explorer · mint transaction',
      };
    }
  }
}

/** One-line summary of where the trace is, for the overview card. */
export function traceNote(s: FundingStatus): string {
  if (s.state === 'settled') {
    return `Complete · ${creditText(s.credits ?? 0)} credits minted`;
  }
  if (isDirectPurchase(s)) {
    return 'Step 1 of 1 · minting credits...';
  }
  const idx = stageIndex(s);
  const notes = [
    `transaction pending on ${s.chainName || 'Solana'}`,
    `deposit seen at block ${num(s.ethBlock ?? 0)}`,
    `height ${num(s.attestedHeight ?? 0)} attested · minting credits${s.ccTx ? ` · ${short(s.ccTx)}` : ''}`,
  ];
  return `Step ${idx + 1} of 3 · ${notes[idx]}`;
}
