import { blockUrl, buildSteps, explorerTxUrl, isDirectPurchase, isSettled, mid, stageIndex, traceNote } from './trace';
import { localTime } from './relative-time';
import { FundingStatus } from './api';

/** A helper to build a FundingStatus with sensible defaults.
 *
 *  The default is a legacy imported row: chain key 1, a source chain named
 *  Ethereum, the three-step proof anatomy. Chain 0 is Solana, the only chain
 *  this deployment writes to, and the tests that exercise it set it
 *  explicitly - so `Ethereum` here never describes the live deployment. */
function makeStatus(overrides: Partial<FundingStatus> = {}): FundingStatus {
  return {
    purchaseId: 'p1',
    state: 'pending',
    ethTx: '0x9f2ad4e1c88b7605a3f0d2914e77bb60c4a1de0938f5c2b7a04e61d3be04c115',
    ethBlock: null,
    attestedHeight: null,
    chainTip: null,
    builderCeiling: null,
    ccTx: null,
    ccBlock: null,
    credits: null,
    amountUsd: 250,
    token: 'mUSDC',
    startedAt: '2026-08-22T20:00:00Z',
    updatedAt: '2026-08-22T20:00:00Z',
    chainKey: 1,
    chainName: 'Ethereum',
    ...overrides,
  };
}

describe('funding trace', () => {
  it('starts with every step queued except the first', () => {
    const s = makeStatus(); // ethBlock is null → step 0 live, rest pending
    const steps = buildSteps(s);
    expect(steps[0].state).toBe('live');
    expect(steps.slice(1).every((st) => st.state === 'pending')).toBe(true);
  });

  it('never marks a step done before the one before it', () => {
    const scenarios: Partial<FundingStatus>[] = [
      {},
      { ethBlock: 100 },
      { ethBlock: 100, attestedHeight: 100 },
      { ethBlock: 100, attestedHeight: 100, builderCeiling: 100 },
      { ethBlock: 100, attestedHeight: 100, builderCeiling: 100, ccTx: '0xabc' },
      { ethBlock: 100, attestedHeight: 100, builderCeiling: 100, ccTx: '0xabc', state: 'settled' },
    ];
    for (const overrides of scenarios) {
      const steps = buildSteps(makeStatus(overrides));
      for (let i = 1; i < steps.length; i++) {
        if (steps[i].state !== 'pending') {
          expect(steps[i - 1].state).toBe('done');
        }
      }
    }
  });

  it('groups the three steps into Ethereum, proof layer and Solana', () => {
    const s = makeStatus({ state: 'settled', ethBlock: 100, attestedHeight: 100, builderCeiling: 100, ccTx: '0xabc' });
    const steps = buildSteps(s);
    expect(steps.map((st) => st.lane)).toEqual(['eth', 'proof', 'cc']);
    expect(steps.filter((st) => st.laneStart).length).toBe(3);
  });

  it('links the deposit and the mint, and never the attestation frontier', () => {
    const s = makeStatus({ state: 'settled', ethBlock: 100, attestedHeight: 100, builderCeiling: 100, ccTx: '0xabc', ccBlock: 50 });
    const steps = buildSteps(s, 'https://scan.example.com');
    expect(steps[0].linkHref).toMatch(/^https:\/\//);
    expect(steps[2].linkHref).toContain('explorer.solana.com/tx/0xabc');
    expect(steps[1].linkHref).toBe('');
    expect(steps[1].linkLabel).toBe('');
  });

  it('completes only once the mint has landed', () => {
    expect(isSettled(makeStatus())).toBe(false);
    expect(isSettled(makeStatus({ state: 'confirmed' }))).toBe(false);
    expect(isSettled(makeStatus({ state: 'settled' }))).toBe(true);
  });

  it('builder lagging below the attested height does not hold the trace back', () => {
    const s = makeStatus({
      ethBlock: 100,
      attestedHeight: 200,
      chainTip: 250,
      builderCeiling: 150, // below attestedHeight (200)
    });
    const steps = buildSteps(s);
    // The deposit is seen and the height is attested; the mint is live.
    expect(steps[0].state).toBe('done');
    expect(steps[1].state).toBe('done');
    expect(steps[2].state).toBe('live');
    expect(stageIndex(s)).toBe(2);
  });

  it('has no link for a step whose fact is still unknown', () => {
    const s = makeStatus({ ethBlock: 100 });
    const steps = buildSteps(s, 'https://scan.example.com');
    expect(steps[0].linkHref).toContain('/block/100');
    expect(steps[1].linkHref).toBe('');
    expect(steps[2].linkHref).toBe('');
  });

  it('links the mint to the Solana explorer on the devnet cluster', () => {
    const s = makeStatus({ state: 'settled', ethBlock: 100, attestedHeight: 100, builderCeiling: 100, ccTx: '0xabc', ccBlock: 50 });
    expect(buildSteps(s)[2].linkHref).toBe('https://explorer.solana.com/tx/0xabc?cluster=devnet');
  });

  // The attested height is a height on the deposit chain. No dashboard
  // exists for it, so the step carries no link at all rather than pointing
  // at a host nobody operates.
  it('never links the attestation frontier to a block explorer', () => {
    const s = makeStatus({ state: 'settled', ethBlock: 11553471, attestedHeight: 11553580, builderCeiling: 11553590, ccTx: '0xabc', ccBlock: 5362890 });
    const steps = buildSteps(s);
    expect(steps[1].linkHref).toBe('');
  });

  it('explorer links use the source chain base, not a hardcoded host', () => {
    // The eth-lane link is built from the source explorer base threaded in
    // from /console/chains; passing a fake base proves nothing is hardcoded.
    const s = makeStatus({ ethBlock: 100 });
    const steps = buildSteps(s, 'https://ethereum.example.com');
    expect(steps[0].linkHref).toContain('ethereum.example.com');
    expect(steps[0].linkHref).toContain('/block/100');
  });

  it('leaves the eth-lane link empty when no source explorer is known', () => {
    const s = makeStatus({ ethBlock: 100 });
    const steps = buildSteps(s);
    expect(steps[0].linkHref).toBe('');
  });

  it('shows the source chain name as the step-0 network meta', () => {
    const s = makeStatus({ state: 'confirmed', ethBlock: 100, chainName: 'Ethereum' });
    const steps = buildSteps(s, 'https://scan.example.com');
    expect(steps[0].meta.find((m) => m.label === 'Network')?.value).toBe('Ethereum');
  });

  it('stageIndex returns the first non-done step', () => {
    expect(stageIndex(makeStatus())).toBe(0); // ethBlock null → deposit unseen
    expect(stageIndex(makeStatus({ ethBlock: 100 }))).toBe(1); // attestedHeight null → unattested
    expect(stageIndex(makeStatus({ ethBlock: 100, attestedHeight: 100 }))).toBe(2); // not settled → minting
    expect(stageIndex(makeStatus({ ethBlock: 100, attestedHeight: 100, ccTx: '0xabc' }))).toBe(2); // ccTx alone does not settle
    expect(stageIndex(makeStatus({ state: 'settled', ethBlock: 100, attestedHeight: 100, ccTx: '0xabc' }))).toBe(3);
  });

  it('traceNote reports the current stage', () => {
    expect(traceNote(makeStatus())).toContain('Step 1 of 3');
    expect(traceNote(makeStatus({ ethBlock: 100, attestedHeight: 100 }))).toContain('Step 3 of 3');
    expect(traceNote(makeStatus({ state: 'settled', credits: 250_000 }))).toContain('Complete');
  });

  // ---- direct purchases (Solana, chain 0) ----------------------------

  it('a direct purchase (chain 0) builds one step in the single credit lane', () => {
    // A real Solana deposit: the paying signature is also the mint id, and
    // the source block is the slot the transfer landed in.
    const sig = '0xdirectabcdef1234567890abcdef';
    const s = makeStatus({
      state: 'settled', chainKey: 0, chainName: 'Solana',
      ethTx: sig, ethBlock: 404, ccTx: sig, ccBlock: 404, credits: 250_000,
    });
    const steps = buildSteps(s);
    expect(steps.length).toBe(1);
    expect(steps[0].lane).toBe('cc');
    expect(steps[0].state).toBe('done');
    expect(steps[0].laneStart).toBe(true);
    expect(steps[0].title).toBe('Credits minted');
    expect(steps[0].linkHref).toContain(`explorer.solana.com/tx/${sig}`);
    expect(steps[0].meta.some((m) => m.label === 'Transaction' && m.value.includes('0xdirectab'))).toBe(true);
    expect(steps[0].meta.some((m) => m.label === 'Slot' && m.value === '404')).toBe(true);
    expect(steps[0].meta.some((m) => m.label === 'Credited' && m.value.includes('250,000'))).toBe(true);
  });

  it('a just-sent direct deposit renders live, never as minted credits', () => {
    const s = makeStatus({
      state: 'pending', chainKey: 0, chainName: 'Solana',
      ethTx: '0xjustsent', ethBlock: 404, ccTx: null, ccBlock: null, credits: null,
    });
    const steps = buildSteps(s);
    expect(steps.length).toBe(1);
    expect(steps[0].state).toBe('live');
    expect(steps[0].title).toBe('Deposit sent');
    expect(steps[0].linkHref).toContain('explorer.solana.com/tx/0xjustsent');
    expect(steps[0].meta.find((m) => m.label === 'Slot')?.value).toBe('-');
    expect(steps[0].confirmation).toEqual({ kind: 'meter', percent: 50, text: 'Minting credits' });
  });

  it('a legacy purchase (chain key 1) builds three steps', () => {
    const s = makeStatus({
      state: 'settled', ethBlock: 100, attestedHeight: 100,
      builderCeiling: 100, ccTx: '0xabc', ccBlock: 50, credits: 250_000,
    });
    expect(buildSteps(s).length).toBe(3);
  });

  it('a settled proven purchase renders the three proof-anatomy titles, both explorer links and the wire timestamps', () => {
    const s = makeStatus({
      state: 'settled', ethBlock: 100, attestedHeight: 100, chainTip: 150,
      ccTx: '0xabc', ccBlock: 50, credits: 250_000,
      startedAt: '2026-08-22T20:00:00Z', updatedAt: '2026-08-22T20:05:00Z',
    });
    const steps = buildSteps(s, 'https://scan.example.com');
    expect(steps.map((st) => st.title)).toEqual([
      'Deposit seen on Ethereum slot 100',
      'Height attested by the watcher (100)',
      'Deposit confirmed · credits minted',
    ]);
    expect(steps[0].linkHref).toBe('https://scan.example.com/block/100');
    expect(steps[2].linkHref).toContain('/tx/0xabc');
    expect(steps[0].when).toBe(localTime('2026-08-22T20:00:00Z'));
    expect(steps[1].when).toBe('');
    expect(steps[2].when).toBe(localTime('2026-08-22T20:05:00Z'));
  });

  it('a mempool deposit keeps its transaction link, shows no timestamp and titles without a block', () => {
    const s = makeStatus(); // ethBlock null, pending
    const steps = buildSteps(s, 'https://scan.example.com');
    expect(steps[0].title).toBe('Deposit seen on Ethereum');
    expect(steps[0].when).toBe('');
    expect(steps[0].linkHref).toBe(`https://scan.example.com/tx/${s.ethTx}`);
    expect(steps[0].meta.find((m) => m.label === 'Slot')?.value).toBe('-');
  });

  it('a direct and a legacy purchase are distinguishable in a mixed list', () => {
    const direct = makeStatus({
      purchaseId: 'd1', state: 'settled', chainKey: 0, chainName: 'Solana',
      ccTx: '0xdirect1', ccBlock: 77, credits: 250_000,
    });
    const legacy = makeStatus({
      purchaseId: 'p1', state: 'settled', ethBlock: 100,
      attestedHeight: 100, builderCeiling: 100, ccTx: '0xabc',
      ccBlock: 50, credits: 250_000,
    });
    const list = [direct, legacy];
    expect(list.filter((s) => isDirectPurchase(s)).length).toBe(1);
    expect(buildSteps(direct).length).toBe(1);
    expect(buildSteps(legacy).length).toBe(3);
  });

  it('isDirectPurchase keys off the single chain, not the source block', () => {
    // A Solana deposit carries a real slot, so a non-zero source block must
    // not read as still-being-proven.
    expect(isDirectPurchase(makeStatus({ chainKey: 0, ethBlock: 40400 }))).toBe(true);
    expect(isDirectPurchase(makeStatus({ chainKey: 0, ethBlock: null }))).toBe(true);
    // A legacy row on another chain keeps the three-step anatomy.
    expect(isDirectPurchase(makeStatus({ chainKey: 1, ethBlock: 100 }))).toBe(false);
    expect(isDirectPurchase(makeStatus({ chainKey: 1 }))).toBe(false);
  });
});

describe('explorer URL helpers', () => {
  it('blockUrl strips a /tx/ suffix and appends /block/{raw}', () => {
    expect(blockUrl('https://etherscan.io/tx/', 11581717)).toBe(
      'https://etherscan.io/block/11581717',
    );
  });

  it('blockUrl with a clean base appends /block/{raw}', () => {
    expect(blockUrl('https://scan.example.com', 100)).toBe('https://scan.example.com/block/100');
  });

  it('blockUrl with empty base returns empty string', () => {
    expect(blockUrl('', 42)).toBe('');
  });

  it('blockUrl never injects locale separators', () => {
    const url = blockUrl('https://scan.example.com', 9999999);
    expect(url).toMatch(/^https:\/\/[^/]+\/block\/\d+$/);
  });

  it('explorerTxUrl with a /tx/-suffixed base has no double slash', () => {
    expect(explorerTxUrl('https://etherscan.io/tx/', '0xabc')).toBe(
      'https://etherscan.io/tx/0xabc',
    );
  });

  it('explorerTxUrl with a clean base inserts /tx/', () => {
    expect(explorerTxUrl('https://scan.example.com', '0xabc')).toBe('https://scan.example.com/tx/0xabc');
  });

  it('explorerTxUrl with empty hash returns empty string', () => {
    expect(explorerTxUrl('https://scan.example.com', '')).toBe('');
  });
});

describe('funding trace - block URL integration', () => {
  it('large block number with /tx/-suffixed base: raw number in URL, formatted in label', () => {
    const s = makeStatus({ ethBlock: 11581717 });
    const steps = buildSteps(s, 'https://etherscan.io/tx/');
    expect(steps[0].linkHref).toBe('https://etherscan.io/block/11581717');
    expect(steps[0].linkLabel).toContain('11,581,717');
  });

  it('mempool tx with /tx/-suffixed base: bare hash, no double slash', () => {
    const s = makeStatus(); // ethBlock null → mempool
    const steps = buildSteps(s, 'https://etherscan.io/tx/');
    expect(steps[0].linkHref).toBe(`https://etherscan.io/tx/${s.ethTx}`);
  });

  it('no linkHref contains a double slash outside the scheme (fully settled, /tx/ base)', () => {
    const s = makeStatus({
      state: 'settled', ethBlock: 11581717, attestedHeight: 11581800,
      builderCeiling: 11581800, ccTx: '0xdeadbeef', ccBlock: 5362890,
    });
    const steps = buildSteps(s, 'https://etherscan.io/tx/');
    for (const step of steps) {
      if (step.linkHref === '') continue;
      const afterScheme = step.linkHref.replace(/^https:\/\//, '');
      expect(afterScheme).not.toContain('//');
    }
  });
});

describe('mid', () => {
  it('passes short values through unchanged', () => {
    expect(mid('0x7a3F…9C21', 18)).toBe('0x7a3F…9C21');
    expect(mid('abc', 3)).toBe('abc');
    expect(mid('', 18)).toBe('');
  });

  it('caps a 42-char address at 18 chars with head and tail kept', () => {
    const addr = '0x1234567890abcdef1234567890abcdef12345678';
    const out = mid(addr, 18);
    expect(out.length).toBe(18);
    expect(out.startsWith(addr.slice(0, 9))).toBe(true);
    expect(out.endsWith(addr.slice(-8))).toBe(true);
    expect(out).toContain('…');
  });

  it('splits an odd cap evenly and an even cap one extra to the head', () => {
    const text = 'abcdefghijklmnopqrst'; // 20 chars
    expect(mid(text, 17)).toBe(`${text.slice(0, 8)}…${text.slice(-8)}`); // odd 17: head 8, tail 8
    expect(mid(text, 16)).toBe(`${text.slice(0, 8)}…${text.slice(-7)}`); // even 16: head 8, tail 7
  });

  it('returns the raw prefix without an ellipsis when max is under 3', () => {
    expect(mid('abcdef', 2)).toBe('ab');
    expect(mid('abcdef', 0)).toBe('');
  });
});
