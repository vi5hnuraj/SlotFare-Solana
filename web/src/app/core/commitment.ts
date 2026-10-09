import { bytesToHex, concatHex, hexToBytes, keccak256, stringToBytes, toHex, type Hex } from 'viem';
import type { UsageCharge, UsageProof } from './api';

/**
 * The Merkle commitment that links a settled charge to its batch, and the
 * client-side verification of the inclusion proof the metering server
 * serves for it. Pure functions over 32-byte hashes; no I/O.
 *
 * Scheme (the one the server's proofs follow):
 * - leaf: keccak256 of the canonical charge string
 *   `<occurredAt>|<keyId>|<model>|<prompt>|<cached>|<completion>|<credits>`;
 *   a null token field becomes an empty token, and credits are the integer
 *   credits the row carries on the wire (micro-credits), so no float ever
 *   enters a hash.
 * - internal node: keccak256 of its two 32-byte children concatenated,
 *   left first.
 * - levels pair left-to-right; an odd count carries its last node up
 *   unchanged and re-pairs at the next level.
 * - proof walk: start at the leaf hash; at each level pair with
 *   `path[level]` - left child when the index is even, right child when
 *   odd - then halve the index. A level whose last node was carried up
 *   unchanged (an odd count) hashes nothing: its path entry is the
 *   all-zero CARRY sentinel, which the walk skips, so a survivor leaf
 *   still lands on the root.
 */

/** The path entry for a level where the tracked node was carried up
 *  unchanged. All-zero 32 bytes: never a real keccak output, and the
 *  marker the walk uses to skip that level's hash. */
const CARRY = '0x0000000000000000000000000000000000000000000000000000000000000000';

/** One null token field renders as an empty token in the canonical string. */
const token = (v: number | null): string => (v === null ? '' : `${v}`);

/** The leaf hash of one settled charge. Stable across calls. */
export function usageLeafHash(row: UsageCharge): string {
  const s = [
    row.occurredAt,
    row.keyId,
    row.model,
    token(row.promptTokens),
    token(row.cachedTokens),
    token(row.completionTokens),
    `${row.credits}`,
  ].join('|');
  return keccak256(stringToBytes(s));
}

/** Pair one level of 32-byte hashes into the next; an odd count carries
 *  its last node up unchanged. */
function nextLevel(level: Hex[]): Hex[] {
  const next: Hex[] = [];
  for (let i = 0; i + 1 < level.length; i += 2) {
    next.push(keccak256(concatHex([level[i], level[i + 1]])));
  }
  if (level.length % 2 === 1) next.push(level[level.length - 1]);
  return next;
}

/** The commitment root over a batch's leaves. An empty batch commits the
 *  hash of the empty string, so "no charges" is a value, not an absence. */
export function merkleRoot(leafHashes: string[]): string {
  let level =
    leafHashes.length > 0
      ? leafHashes as Hex[]
      : [keccak256(stringToBytes(''))];
  while (level.length > 1) level = nextLevel(level);
  return level[0];
}

/** Walk one leaf up through its sibling path, leaf to root. A CARRY entry
 *  skips that level's hash (the node was carried up unchanged) but still
 *  advances the index. A path that does not match the tree's shape lands
 *  somewhere else: this never throws. */
export function walkProof(leaf: string, index: number, path: string[]): string {
  let h = leaf as Hex;
  let i = index;
  for (const sibling of path) {
    if (sibling !== CARRY) {
      h =
        i % 2 === 0
          ? keccak256(concatHex([h, sibling as Hex]))
          : keccak256(concatHex([sibling as Hex, h]));
    }
    i = Math.floor(i / 2);
  }
  return h;
}

/** The sibling path of leaf `index` in a tree built from these leaves,
 *  bottom up, leaf to root. The inverse of `walkProof`: walking any leaf
 *  with its own path reproduces `merkleRoot(leafHashes)` - including a
 *  survivor on an odd-count level, whose entry is CARRY. */
export function siblingPath(leafHashes: string[], index: number): string[] {
  const path: string[] = [];
  let level = leafHashes as Hex[];
  let i = index;
  while (level.length > 1) {
    // An odd level carries its last node up unchanged: that node has no
    // sibling to hash with here, so mark the level and advance. Otherwise
    // the sibling is the paired node, left or right of the tracked index.
    if (level.length % 2 === 1 && i === level.length - 1) {
      path.push(CARRY);
    } else {
      const sibling = i % 2 === 0 ? i + 1 : i - 1;
      path.push(level[sibling]);
    }
    i = Math.floor(i / 2);
    level = nextLevel(level);
  }
  return path;
}

/**
 * Verify one served inclusion proof: recompute the leaf from the proof's
 * own charge fields, walk it up the sibling path, and compare against BOTH
 * the root the proof claims and the root the row's commitment carries.
 * Either mismatch fails - a proof that walks to the wrong root proves
 * nothing about this charge.
 */
export function verifyUsageProof(proof: UsageProof, expectedRoot: string): boolean {
  if (proof.canonical) {
    const c = proof.canonical;
    return verifyCanonicalProof(c.leaf, c.leafHash, c.steps, proof.root, expectedRoot);
  }
  const walked = walkProof(usageLeafHash(proof), proof.index, proof.path);
  return walked === proof.root && walked === expectedRoot;
}


// ---- the server's canonical scheme (slotfared internal/meter/usageroot.go) ----

/** The ten words slotfared hashes for one charge, as GET /usage/{id}/proof
 *  reports them. Recomputed client-side so the served leaf hash is checked,
 *  not trusted. */
export interface CanonicalLeaf {
  id: number;
  accountId: string;
  ts: number;
  service: string;
  model: string;
  promptTokens: number;
  cachedTokens: number;
  completionTokens: number;
  creditsMicro: number;
  status: string;
}

/** One combine on the way up: the sibling hash and which side it sat on. */
export interface ProofStep {
  hash: string;
  direction: 'left' | 'right';
}

const hex32 = (h: string): Hex => (h.startsWith('0x') ? h : `0x${h}`) as Hex;

/** A uint64 as one 32-byte big-endian word. */
function uintWord(v: number): Uint8Array {
  const out = new Uint8Array(32);
  let x = BigInt(v);
  for (let i = 31; i >= 24; i--) {
    out[i] = Number(x & 0xffn);
    x >>= 8n;
  }
  return out;
}

/** slotfared's abiStringWord: 32-byte words, each a 4-byte big-endian length
 *  then up to 28 bytes of UTF-8, zero-padded; the empty string is one zero word. */
function abiStringWords(s: string): Uint8Array {
  const b = stringToBytes(s);
  let n = Math.ceil(b.length / 32);
  if (n === 0) n = 1;
  const out = new Uint8Array(n * 32);
  for (let i = 0; i < n; i++) {
    const seg = b.slice(i * 32, i * 32 + 32);
    const len = seg.length;
    out[i * 32] = (len >>> 24) & 0xff;
    out[i * 32 + 1] = (len >>> 16) & 0xff;
    out[i * 32 + 2] = (len >>> 8) & 0xff;
    out[i * 32 + 3] = len & 0xff;
    out.set(seg, i * 32 + 4);
  }
  return out;
}

/** keccak256 over the ten canonical words, exactly as slotfared's UsageLeaf. */
export function canonicalLeafHash(l: CanonicalLeaf): Hex {
  const parts: Uint8Array[] = [
    uintWord(l.id),
    hexToBytes(hex32(l.accountId)),
    uintWord(l.ts),
    abiStringWords(l.service),
    abiStringWords(l.model),
    uintWord(l.promptTokens),
    uintWord(l.cachedTokens),
    uintWord(l.completionTokens),
    uintWord(l.creditsMicro),
    abiStringWords(l.status),
  ];
  const total = parts.reduce((n, p) => n + p.length, 0);
  const buf = new Uint8Array(total);
  let o = 0;
  for (const p of parts) {
    buf.set(p, o);
    o += p.length;
  }
  return keccak256(bytesToHex(buf));
}

/** Walk leaf to root through directed steps; no steps means the leaf is the root. */
export function walkSteps(leaf: string, steps: ProofStep[]): Hex {
  let h = hex32(leaf);
  for (const s of steps) {
    const sib = hex32(s.hash);
    h = s.direction === 'left' ? keccak256(concatHex([sib, h])) : keccak256(concatHex([h, sib]));
  }
  return h;
}

const same = (a: string, b: string): boolean =>
  a.replace(/^0x/, '').toLowerCase() === b.replace(/^0x/, '').toLowerCase();

/** True when the recomputed leaf matches the served leaf hash and walking the
 *  steps reaches both the served batch root and the root shown on the row. */
export function verifyCanonicalProof(
  leaf: CanonicalLeaf,
  leafHash: string,
  steps: ProofStep[],
  batchRoot: string,
  expectedRoot: string,
): boolean {
  const computed = canonicalLeafHash(leaf);
  if (!same(computed, leafHash)) return false;
  const root = walkSteps(computed, steps);
  return same(root, batchRoot) && same(root, expectedRoot);
}
