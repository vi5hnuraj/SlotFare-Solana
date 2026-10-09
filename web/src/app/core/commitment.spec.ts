import { keccak256, stringToBytes } from 'viem';
import {
  merkleRoot,
  siblingPath,
  usageLeafHash,
  verifyUsageProof,
  walkProof,
} from './commitment';
import { canonicalLeafHash, verifyCanonicalProof, walkSteps } from './commitment';
import { USAGE } from './mock-data';
import type { UsageCharge, UsageProof } from './api';

/** A fixed charge: the leaf's canonical string is asserted against viem
 *  directly, so the scheme is pinned, not just self-consistent. */
const charge = (over: Partial<UsageCharge> = {}): UsageCharge => ({
  occurredAt: '2026-08-25T14:00:00Z',
  keyId: '7Qk4',
  model: 'llama-3.2-3b',
  promptTokens: 100,
  cachedTokens: 0,
  completionTokens: 50,
  credits: 10_000,
  ...over,
});

/** The fixture's settled rows, as charges in wire units. */
const fixtureCharges = USAGE.filter((r) => r.commitment !== null).map(
  (r): UsageCharge => ({
    occurredAt: r.time,
    keyId: r.key,
    model: r.model,
    promptTokens: r.prompt,
    cachedTokens: r.cached,
    completionTokens: r.completion,
    credits: Math.round(r.credits * 10_000),
  }),
);

describe('usageLeafHash', () => {
  it('is stable across calls and hashes the canonical charge string', () => {
    const c = charge();
    expect(usageLeafHash(c)).toBe(usageLeafHash(c));
    expect(usageLeafHash(c)).toBe(
      keccak256(stringToBytes('2026-08-25T14:00:00Z|7Qk4|llama-3.2-3b|100|0|50|10000')),
    );
  });

  it('maps null token fields to empty tokens', () => {
    const c = charge({ promptTokens: null, cachedTokens: null, completionTokens: null });
    expect(usageLeafHash(c)).toBe(
      keccak256(stringToBytes('2026-08-25T14:00:00Z|7Qk4|llama-3.2-3b||||10000')),
    );
  });

  it('changes with every charge field', () => {
    const base = usageLeafHash(charge());
    const variants: UsageCharge[] = [
      charge({ occurredAt: '2026-08-25T14:00:01Z' }),
      charge({ keyId: '2Mv9' }),
      charge({ model: 'qwen3-8-27b' }),
      charge({ promptTokens: 101 }),
      charge({ cachedTokens: 1 }),
      charge({ completionTokens: 51 }),
      charge({ credits: 10_001 }),
    ];
    for (const v of variants) {
      expect(usageLeafHash(v)).not.toBe(base);
    }
  });
});

describe('merkleRoot', () => {
  it('is the leaf itself for a one-leaf batch', () => {
    const leaves = [usageLeafHash(charge())];
    expect(merkleRoot(leaves)).toBe(leaves[0]);
  });

  it('pairs two leaves left first', () => {
    const a = usageLeafHash(charge());
    const b = usageLeafHash(charge({ keyId: '2Mv9' }));
    expect(merkleRoot([a, b])).not.toBe(a);
    expect(merkleRoot([a, b])).not.toBe(b);
    // Order matters: swapping the pair changes the root.
    expect(merkleRoot([a, b])).not.toBe(merkleRoot([b, a]));
  });

  it('carries an odd level up and re-pairs at the next level', () => {
    const c = charge();
    const leaves = [
      usageLeafHash(c),
      usageLeafHash(charge({ keyId: '2Mv9' })),
      usageLeafHash(charge({ model: 'qwen3-8-27b' })),
    ];
    const root = merkleRoot(leaves);
    // The third leaf survives the first level unchanged, then pairs with
    // the first node.
    const firstPair = walkProof(leaves[0], 0, [leaves[1]]);
    expect(root).toBe(walkProof(firstPair, 0, [leaves[2]]));
  });

  it('agrees with the fixture settlement facts the demo serves', () => {
    const leaves = fixtureCharges.map(usageLeafHash);
    const root = merkleRoot(leaves);
    for (const r of USAGE.filter((x) => x.commitment !== null)) {
      expect(r.commitment!.root).toBe(root);
    }
  });
});

describe('walkProof / siblingPath', () => {
  it('walks every fixture leaf back to the batch root', () => {
    const leaves = fixtureCharges.map(usageLeafHash);
    const root = merkleRoot(leaves);
    for (let i = 0; i < leaves.length; i += 1) {
      expect(walkProof(leaves[i], i, siblingPath(leaves, i))).toBe(root);
    }
  });

  it('fails on a corrupted sibling hash', () => {
    const leaves = fixtureCharges.map(usageLeafHash);
    const root = merkleRoot(leaves);
    const path = siblingPath(leaves, 0);
    const corrupted = [...path];
    corrupted[0] = keccak256(stringToBytes('someone else'));
    expect(walkProof(leaves[0], 0, corrupted)).not.toBe(root);
  });

  it('fails on a wrong index', () => {
    const leaves = fixtureCharges.map(usageLeafHash);
    const root = merkleRoot(leaves);
    const path = siblingPath(leaves, 0);
    expect(walkProof(leaves[0], 1, path)).not.toBe(root);
  });

  it('walks every leaf of an odd-leaf tree back to the root', () => {
    // Five distinct charges: level 0 is odd (5 -> 3 -> 2 -> 1), so the
    // last node is carried up unchanged on two levels. The survivor leaf
    // must still walk to the root, not to a wrong pairing with itself.
    const leaves = [0, 1, 2, 3, 4].map((n) => usageLeafHash(charge({ keyId: `K${n}` })));
    const root = merkleRoot(leaves);
    for (let i = 0; i < leaves.length; i += 1) {
      expect(walkProof(leaves[i], i, siblingPath(leaves, i))).toBe(root);
    }
    // The survivor's path is marked: its first entry is the all-zero CARRY
    // sentinel, not a sibling to hash against. (Leaf 4 -> level 3 -> 2.)
    const survivorPath = siblingPath(leaves, 4);
    expect(survivorPath[0]).toBe('0x0000000000000000000000000000000000000000000000000000000000000000');
  });

  it('walks every leaf of a longer odd tree back to the root', () => {
    const leaves = [0, 1, 2, 3, 4, 5, 6].map((n) => usageLeafHash(charge({ model: `m${n}` })));
    const root = merkleRoot(leaves);
    for (let i = 0; i < leaves.length; i += 1) {
      expect(walkProof(leaves[i], i, siblingPath(leaves, i))).toBe(root);
    }
  });
});

describe('verifyUsageProof', () => {
  /** A good proof over the fixture tree, for the row at `index`. */
  function goodProof(index: number): UsageProof {
    const c = fixtureCharges[index];
    return {
      ...c,
      index,
      path: siblingPath(fixtureCharges.map(usageLeafHash), index),
      root: merkleRoot(fixtureCharges.map(usageLeafHash)),
    };
  }

  it('verifies a fixture proof against its committed root', () => {
    const proof = goodProof(0);
    const expectedRoot = USAGE.find((r) => r.commitment !== null)!.commitment!.root;
    expect(verifyUsageProof(proof, expectedRoot)).toBe(true);
  });

  it('fails on a corrupted leaf charge field', () => {
    const proof = goodProof(0);
    expect(verifyUsageProof({ ...proof, credits: proof.credits + 1 }, proof.root)).toBe(false);
    expect(verifyUsageProof({ ...proof, keyId: '2Mv9' }, proof.root)).toBe(false);
  });

  it('fails on a corrupted sibling', () => {
    const proof = goodProof(0);
    const path = [...proof.path];
    path[0] = keccak256(stringToBytes('tampered'));
    expect(verifyUsageProof({ ...proof, path }, proof.root)).toBe(false);
  });

  it('fails on a wrong index', () => {
    const proof = goodProof(0);
    expect(verifyUsageProof({ ...proof, index: proof.index + 1 }, proof.root)).toBe(false);
  });

  it('fails on a wrong expected root even when the proof is internally consistent', () => {
    const proof = goodProof(0);
    expect(verifyUsageProof(proof, keccak256(stringToBytes('other root')))).toBe(false);
  });
});


describe("slotfared's canonical scheme", () => {
  // Row 40 of the testnet ledger, committed alone in batch 7 on 2026-09-03:
  // the served leaf hash and the batch root are the same 32 bytes.
  const leaf = {
    id: 40,
    accountId: '0xd87b3fb61c183c3fdc1edf3e49d563c47f3389a990ac9172526e68b80bc08712',
    ts: 1788481715,
    service: 'inference',
    model: 'kimi-k2.6',
    promptTokens: 31,
    cachedTokens: 0,
    completionTokens: 372,
    creditsMicro: 19726,
    status: 'ok',
  };
  const root = '7407b7d6721dda3ba876a56d93aaf3ee6e9a62f3d87bc1d4f7ab9a992a18137b';

  it('recomputes the leaf slotfared committed', () => {
    expect(canonicalLeafHash(leaf)).toBe(`0x${root}`);
  });

  it('a single-row batch verifies with no steps, and fails on a changed charge', () => {
    expect(verifyCanonicalProof(leaf, root, [], root, root)).toBe(true);
    expect(verifyCanonicalProof({ ...leaf, creditsMicro: 19727 }, root, [], root, root)).toBe(false);
  });

  it('walks directed steps on the side the server says', () => {
    const a = canonicalLeafHash(leaf);
    const b = canonicalLeafHash({ ...leaf, id: 41 });
    const viaLeft = walkSteps(b, [{ hash: a, direction: 'left' }]);
    const viaRight = walkSteps(a, [{ hash: b, direction: 'right' }]);
    expect(viaLeft).toBe(viaRight);
  });
});
