import { signal } from '@angular/core';
import { TestBed } from '@angular/core/testing';
import { provideHttpClient, withFetch } from '@angular/common/http';
import {
  HttpTestingController,
  provideHttpClientTesting,
} from '@angular/common/http/testing';
import { WalletService } from './wallet';
import { ApiError, AttestationChainWire, ChainInfo, SlotfareApi, HttpSlotfareApi, MockSlotfareApi, UsageCharge, UsageProof, fromAttestationWire, fromChainWire, fromListingWire, fromMarketListingWire, fromModelWire, fromPurchaseView } from './api';
import { merkleRoot, usageLeafHash, verifyUsageProof } from './commitment';
import { OPENING_BALANCE, OPENING_LOCKED, USAGE, WALLET_ADDRESS } from './mock-data';
import { environment } from '../../environments/environment';

/** URL of a console endpoint, as the HttpClient will request it. Relative
 *  (`environment.apiUrl` is '', the dev proxy), exactly as the request is
 *  issued and recorded by the testing backend. */
const BASE = `${environment.apiUrl}/console`;

/** The account id the mock derives for the fixture wallet address. */
const MOCK_ACCOUNT_ID =
  '0xmock' + WALLET_ADDRESS.toLowerCase().replace(/[^a-z0-9]/g, '').slice(-8);

/** The wire account body both implementations must agree on: integer
 *  micro-credits (10,000 per credit), as the server's `/console/account`
 *  and the balance stream carry them. */
function accountBody() {
  const totalSpent = USAGE.reduce((acc, r) => acc + r.credits, 0);
  return {
    accountId: MOCK_ACCOUNT_ID,
    mintedMicro: OPENING_BALANCE * 10_000,
    settledMicro: Math.round(totalSpent * 10_000),
    pendingMicro: 0,
    lockedMicro: OPENING_LOCKED * 10_000,
    spendableMicro: OPENING_BALANCE * 10_000,
  };
}

/** A minimal WalletService stub: the token for HttpSlotfareApi's headers()
 *  and the address it stamps on the account. */
type WalletStub = {
  token: ReturnType<typeof signal<string | null>>;
  address: ReturnType<typeof signal<string | null>>;
};
function makeWalletStub(): WalletStub {
  return { token: signal<string | null>(null), address: signal<string | null>(WALLET_ADDRESS) };
}

type Flusher = (
  method: string,
  path: string,
  body: object | null,
  status?: number,
) => void;

const noopFlush: Flusher = () => {};

/**
 * The conformance suite, written once and run against BOTH implementations.
 * Every assertion here is a contract: if either implementation fails it,
 * the two have drifted. `getApi` defers resolution to each test so the
 * suite can be registered before the per-test instances exist. `flush`
 * is a no-op for the mock and serves canned JSON for the HTTP implementation.
 */
function runConformance(getApi: () => SlotfareApi, flush: Flusher): void {
  it('account() resolves the wire micro-credits as the whole-credit figures', async () => {
    const api = getApi();
    const p = api.account();
    flush('GET', '/account', accountBody());
    const acct = await p;
    expect(acct.balanceCredits).toBe(OPENING_BALANCE);
    expect(acct.lockedCredits).toBe(OPENING_LOCKED);
    expect(acct.totalSpentCredits).toBe(
      USAGE.reduce((acc, r) => acc + r.credits, 0),
    );
    expect(acct.address).toBe(WALLET_ADDRESS);
    expect(acct.accountId).toBe(MOCK_ACCOUNT_ID);
  });

  it('keys() resolves with at least one key, all requestCount integers', async () => {
    const api = getApi();
    const keysFixture = {
      keys: [
        { prefix: 'k1', label: 'l1', createdAt: '2026-01-01', lastUsed: null, requestCount: 5, revoked: false },
        { prefix: 'k2', label: 'l2', createdAt: '2026-02-02', lastUsed: '2026-03-03', requestCount: 0, revoked: true },
      ],
    };
    const p = api.keys();
    flush('GET', '/keys', keysFixture);
    const keys = await p;
    expect(keys.length).toBeGreaterThanOrEqual(1);
    for (const k of keys) {
      expect(typeof k.id).toBe('string');
      expect(typeof k.label).toBe('string');
      expect(typeof k.createdAt).toBe('string');
      expect(Number.isInteger(k.requestCount)).toBe(true);
      expect(typeof k.revoked).toBe('boolean');
    }
  });

  it('createKey("test") resolves with a non-empty plaintext and the key appears in keys()', async () => {
    const api = getApi();
    const created = { key: 'slf_live_new1…abcd', prefix: 'new1', label: 'test', createdAt: '2026-08-22' };
    const p = api.createKey('test');
    flush('POST', '/keys', created);
    const ck = await p;
    expect(ck.key.label).toBe('test');
    expect(typeof ck.plaintext).toBe('string');
    expect(ck.plaintext.length).toBeGreaterThan(0);

    const p2 = api.keys();
    flush('GET', '/keys', { keys: [{ prefix: 'new1', label: 'test', createdAt: '2026-08-22', lastUsed: null, requestCount: 0, revoked: false }] });
    const keys = await p2;
    expect(keys.some((k) => k.id === ck.key.id && k.label === 'test')).toBe(true);
  });

  it('revokeKey(id) on an existing id resolves and marks the key revoked', async () => {
    const api = getApi();
    const p = api.revokeKey('7Qk4');
    flush('DELETE', '/keys/7Qk4', null, 204);
    await p;

    const p2 = api.keys();
    flush('GET', '/keys', {
      keys: [{ prefix: '7Qk4', label: 'prod-router', createdAt: '2026-06-14', lastUsed: '2 min ago', requestCount: 184_203, revoked: true }],
    });
    const keys = await p2;
    const k = keys.find((x) => x.id === '7Qk4');
    expect(k).toBeDefined();
    expect(k!.revoked).toBe(true);
  });

  it('revokeKey("nope") rejects with ApiError status 404', async () => {
    const api = getApi();
    const p = api.revokeKey('nope');
    flush('DELETE', '/keys/nope', { code: 'not_found' }, 404);
    await expect(p).rejects.toSatisfy(
      (e: unknown) => e instanceof ApiError && e.status === 404,
    );
  });

  it('usage() resolves with rows whose credits are all integers', async () => {
    const api = getApi();
    const pageFixture = {
      usage: [
        { timestamp: '2026-08-22T14:22:07Z', service: 'inference', model: 'qwen3.8-27b', promptTokens: 8412, cachedTokens: 6144, completionTokens: 1206, creditsMicro: 130_000, status: 'ok', keyPrefix: '7Qk4' },
        { timestamp: '2026-08-22T14:21:54Z', service: 'inference', model: 'qwen3.8-27b', promptTokens: 2190, cachedTokens: 0, completionTokens: 884, creditsMicro: 50_000, status: 'ok', keyPrefix: '7Qk4' },
      ],
    };
    const p = api.usage();
    flush('GET', '/usage', pageFixture);
    const page = await p;
    expect(page.rows.length).toBeGreaterThan(0);
    // The cursor is the last row's id or absent; both implementations must
    // surface it as a number or null, never anything else.
    expect(page.nextCursor === null || Number.isInteger(page.nextCursor)).toBe(true);
    for (const row of page.rows) {
      expect(Number.isInteger(row.credits)).toBe(true);
    }
  });

  it('usage() carries the settlement facts of settled rows and none for pending ones', async () => {
    const api = getApi();
    const p = api.usage();
    flush('GET', '/usage', {
      usage: [
        { timestamp: '2026-08-22T14:22:07Z', service: 'inference', model: 'm', promptTokens: 1, cachedTokens: 0, completionTokens: 1, creditsMicro: 100, status: 'ok', keyPrefix: 'k', batchIndex: 3, root: '0xabcdef', commitTx: '0x123456' },
        { timestamp: '2026-08-22T14:21:54Z', service: 'inference', model: 'm', promptTokens: 1, cachedTokens: 0, completionTokens: 1, creditsMicro: 100, status: 'ok', keyPrefix: 'k' },
      ],
    });
    const page = await p;
    // Every implementation serves at least one settled row.
    const settled = page.rows.filter((r) => r.batchIndex !== null);
    expect(settled.length).toBeGreaterThanOrEqual(1);
    for (const r of settled) {
      expect(typeof r.root).toBe('string');
      expect(r.root!.length).toBeGreaterThan(0);
      expect(typeof r.commitTx).toBe('string');
      expect(r.commitTx!.length).toBeGreaterThan(0);
    }
    // A pending row carries none of the three, each mapped to null.
    const pending = page.rows.filter((r) => r.batchIndex === null);
    expect(pending.length).toBeGreaterThanOrEqual(1);
    for (const r of pending) {
      expect(r.root).toBeNull();
      expect(r.commitTx).toBeNull();
    }
  });

  it('usage() reports facets with no placeholder and no duplicate entries', async () => {
    const api = getApi();
    const p = api.usage();
    flush('GET', '/usage', {
      usage: [],
      facets: {
        prefixes: ['', '  ', '7Qk4', '7Qk4', 42],
        models: ['', 'llama-3.2-3b', 'llama-3.2-3b', null],
      },
    });
    const page = await p;
    expect(page.facets).toBeDefined();
    // The '' placeholder belongs to the consuming dropdown, not the wire:
    // neither facet list may carry it, and no value may appear twice.
    for (const list of [page.facets!.prefixes, page.facets!.models]) {
      expect(list).not.toContain('');
      expect(new Set(list).size).toBe(list.length);
    }
  });

  it('usageProof(rowId) serves the server proof and the canonical walk verifies it', async () => {
    const api = getApi();
    // Row 40 of the testnet ledger, committed alone in batch 7 (2026-09-03):
    // the leaf hash is the batch root, and the wire shape is slotfared's.
    const leaf = {
      id: 40, accountId: '0xd87b3fb61c183c3fdc1edf3e49d563c47f3389a990ac9172526e68b80bc08712',
      ts: 1788481715, service: 'inference', model: 'kimi-k2.6',
      promptTokens: 31, cachedTokens: 0, completionTokens: 372, creditsMicro: 19726, status: 'ok',
    };
    const root = '7407b7d6721dda3ba876a56d93aaf3ee6e9a62f3d87bc1d4f7ab9a992a18137b';
    const p = api.usage();
    flush('GET', '/usage', {
      usage: [
        { id: 40, timestamp: '2026-09-04T00:28:35Z', service: 'inference', model: leaf.model, promptTokens: 31, cachedTokens: 0, completionTokens: 372, creditsMicro: 19726, status: 'ok', keyPrefix: 'attk_1ebab1dd', duration_ms: 7053, batchIndex: 7, root, commitTx: '0x6795ac5773d6b362d16abdf053c318f2615ce165d48b39e238f739337b796895' },
      ],
    });
    const page = await p;
    // Shared suite: the mock answers from its own fixture tree, the HTTP
    // client from the flushed wire body; both must verify against the row.
    const row = page.rows.find((r) => r.root !== null)!;
    expect(row).toBeDefined();
    const pp = api.usageProof(row.id);
    flush('GET', `/usage/${row.id}/proof`, {
      settled: true, leaf, leafHash: root, nonce: '3e40d099a7a6d03cd3a5c4471707e6be', proof: [],
      batch: { root, firstUsageId: 40, lastUsageId: 40, credits: 1, txHash: '0x6795ac57', committedAt: 1788481738, batchIndex: 7 },
    });
    const proof = await pp;
    expect(verifyUsageProof(proof, row.root!)).toBe(true);
    expect(verifyUsageProof(proof, 'deadbeef')).toBe(false);
  });

  it('funding("p1") resolves with a valid state enum value', async () => {
    const api = getApi();
    const fundingFixture = {
      purchaseId: 'p1',
      state: 'settled',
      ethTx: '0xtx',
      ethBlock: 100,
      attestedHeight: 100,
      chainTip: 100,
      builderCeiling: 100,
      ccTx: '0xcc',
      ccBlock: 50,
      credits: OPENING_BALANCE,
      amountUsd: 250,
      token: 'mUSDC',
      startedAt: '2026-08-22T20:00:00Z',
      updatedAt: '2026-08-22T20:00:00Z',
    };
    const p = api.funding('p1');
    flush('GET', '/funding/p1', fundingFixture);
    const fs = await p;
    expect(['pending', 'confirmed', 'settled', 'failed']).toContain(fs.state);
    expect(fs.purchaseId).toBe('p1');
  });

  it('fundingList() resolves with an array of FundingStatus', async () => {
    const api = getApi();
    const listFixture = {
      tip: 150,
      ceiling: 120,
      purchases: [
        { sourceTx: '0xabc', sourceBlock: 100, attestedHeight: 0, ceiling: 120, stage: 4, depositId: '1', token: 'mUSDC', amountUsd: 25, startedAt: '2026-08-23T23:00:00Z', credited: false, ccTx: '', ccBlock: 0, creditedAt: '', credits: 0, chainKey: 1, chainName: 'Ethereum' },
      ],
    };
    const p = api.fundingList();
    flush('GET', '/funding', listFixture);
    const list = await p;
    expect(Array.isArray(list)).toBe(true);
    for (const fs of list) {
      expect(['pending', 'confirmed', 'settled', 'failed']).toContain(fs.state);
      expect(typeof fs.purchaseId).toBe('string');
      expect(typeof fs.ethTx).toBe('string');
      expect(Number.isInteger(fs.chainKey)).toBe(true);
      expect(typeof fs.chainName).toBe('string');
    }
  });

  it('chains() resolves with rows carrying integer keys/ids and non-empty strings', async () => {
    const api = getApi();
    const chainsFixture = {
      chains: [
        { chainKey: 1, name: 'Ethereum', chainId: 11155111, deposit: '0xdep-sep', explorer: 'https://ethereum.example.com' },
        { chainKey: 3, name: 'Ethereum', chainId: 1, deposit: '0xdep-main', explorer: 'https://eth.example.com' },
      ],
    };
    const p = api.chains();
    flush('GET', '/chains', chainsFixture);
    const chains = await p;
    expect(chains.length).toBeGreaterThanOrEqual(1);
    for (const c of chains) {
      expect(Number.isInteger(c.chainKey)).toBe(true);
      expect(Number.isInteger(c.chainId)).toBe(true);
      expect(typeof c.name).toBe('string');
      expect(c.name.length).toBeGreaterThan(0);
      expect(typeof c.deposit).toBe('string');
      expect(c.deposit.length).toBeGreaterThan(0);
      expect(typeof c.explorer).toBe('string');
      expect(c.explorer.length).toBeGreaterThan(0);
    }
  });

  it('listings() resolves with rows whose credits and price are integers', async () => {
    const api = getApi();
    const listingsFixture = {
      listings: [
        { listingId: '1', listedTx: '0xlisttx1', credits: 5000, payTokenSymbol: 'mUSD1', payToken: '0x81a4c39b979fd6d71484b35f14799872593f3321', payTokenDecimals: 6, price: 500, matured: false, state: 'open', role: 'selling', listedAt: '2026-08-22T20:00:00Z' },
        { listingId: '0', listedTx: '0xlisttx0', credits: 10000, payTokenSymbol: 'mUSD1', price: 10000, matured: true, state: 'sold', role: 'selling', listedAt: '2026-08-15T09:30:00Z', listedBlock: 40201, maturesAt: 1755100000, soldTx: '0xsaletx0', soldBlock: 40207, soldAt: '2026-08-15T11:45:00Z' },
      ],
    };
    const p = api.listings();
    flush('GET', '/listings', listingsFixture);
    const page = await p;
    expect(Array.isArray(page.rows)).toBe(true);
    // The cursor is the last row's id or absent; both implementations must
    // surface it as a number or null, never anything else.
    expect(page.nextCursor === null || Number.isInteger(page.nextCursor)).toBe(true);
    for (const l of page.rows) {
      expect(typeof l.id).toBe('string');
      expect(typeof l.listedTx).toBe('string');
      expect(l.listedTx.length).toBeGreaterThan(0);
      expect(['open', 'cancelled', 'sold']).toContain(l.state);
      expect(Number.isInteger(l.credits)).toBe(true);
      expect(Number.isInteger(l.price)).toBe(true);
      expect(typeof l.matured).toBe('boolean');
    }
    // The pay-token address and decimals pass through the adapter. The mock
    // serves the demo mint while the HTTP path serves the fixture's 0x
    // address, so the contract is the shape: a non-empty base58 or hex
    // address, six decimals.
    const addr = page.rows[0].tokenAddress ?? '';
    expect(addr.length).toBeGreaterThan(0);
    expect(/^(0x[0-9a-fA-F]{6,}|[1-9A-HJ-NP-Za-km-z]{8,})$/.test(addr)).toBe(true);
    expect(page.rows[0].tokenDecimals).toBe(6);
    // The open row carries no trade facts: the adapter leaves them undefined
    // for both the mock's own open demo row and the HTTP fixture's open row.
    expect(page.rows[0].listedBlock).toBeUndefined();
    expect(page.rows[0].soldTx).toBeUndefined();
    expect(page.rows[0].soldBlock).toBeUndefined();
    expect(page.rows[0].cancelledTx).toBeUndefined();
  });

  it('market() resolves with rows whose id is a string and matured a boolean', async () => {
    const api = getApi();
    // Copied verbatim from the spec's example response for
    // `GET /console/market` (attest-rpc-docs console.md).
    const marketFixture = JSON.parse(`{ "market": [
      {
        "listingId": 3,
        "credits": 300,
        "payToken": "0x3333...3333",
        "payTokenSymbol": "USDT",
        "price": 120,
        "maturesAt": 1753372800,
        "matured": false,
        "mine": false
      }
    ] }`);
    const p = api.market();
    flush('GET', '/market', marketFixture);
    const list = await p;
    expect(Array.isArray(list)).toBe(true);
    for (const l of list) {
      expect(typeof l.id).toBe('string');
      expect(Number.isInteger(l.credits)).toBe(true);
      expect(Number.isInteger(l.price)).toBe(true);
      expect(typeof l.token).toBe('string');
      expect(l.token.length).toBeGreaterThan(0);
      expect(typeof l.matured).toBe('boolean');
      expect(typeof l.mine).toBe('boolean');
      expect(Number.isInteger(l.maturesAt)).toBe(true);
    }
  });

  it('models() resolves with rows carrying integer per-million pricing', async () => {
    const api = getApi();
    const modelsFixture = {
      models: [
        { id: 'llama-3.2-3b', name: 'Llama 3.2 3B', pricing: { uncachedInput: 10, cachedInput: 1, output: 40 } },
        { id: 'qwen3-8-27b', name: 'Qwen3 27B', pricing: { uncachedInput: 100, cachedInput: 10, output: 300 } },
      ],
    };
    const p = api.models();
    flush('GET', '/models', modelsFixture);
    const { models: list, baseUrl } = await p;
    expect(Array.isArray(list)).toBe(true);
    expect(typeof baseUrl).toBe('string');
    for (const m of list) {
      expect(typeof m.id).toBe('string');
      expect(m.id.length).toBeGreaterThan(0);
      expect(typeof m.name).toBe('string');
      expect(Number.isInteger(m.pricing.uncachedInput)).toBe(true);
      expect(Number.isInteger(m.pricing.cachedInput)).toBe(true);
      expect(Number.isInteger(m.pricing.output)).toBe(true);
    }
  });

  it('attestation() resolves rows with a string name, boolean known and integer lags', async () => {
    const api = getApi();
    const attestFixture = {
      chains: [
        { chainKey: 1, name: 'Ethereum', known: true, attestedTip: 1_000_000, head: 1_000_001, lagBlocks: 1, lagSeconds: 12 },
        { chainKey: 3, name: 'Ethereum', known: false },
      ],
    };
    const p = api.attestation();
    flush('GET', '/attestation', attestFixture);
    const list = await p;
    expect(Array.isArray(list)).toBe(true);
    for (const c of list) {
      expect(typeof c.name).toBe('string');
      expect(typeof c.known).toBe('boolean');
      expect(Number.isInteger(c.chainKey)).toBe(true);
      expect(Number.isInteger(c.attestedTip)).toBe(true);
      expect(Number.isInteger(c.head)).toBe(true);
      expect(Number.isInteger(c.lagBlocks)).toBe(true);
      expect(Number.isInteger(c.lagSeconds)).toBe(true);
    }
  });

  it('verify(addr, "n", "") rejects with ApiError status 401', async () => {
    const api = getApi();
    const p = api.verify(WALLET_ADDRESS, 'n', '');
    flush('POST', '/session/verify', { code: 'invalid_signature' }, 401);
    await expect(p).rejects.toSatisfy(
      (e: unknown) => e instanceof ApiError && e.status === 401,
    );
  });
}

describe('SlotfareApi', () => {
  // ------------------------------------------------------------------
  // a) Conformance suite: run against BOTH implementations
  // ------------------------------------------------------------------

  describe('MockSlotfareApi', () => {
    let api: MockSlotfareApi;
    beforeEach(() => {
      api = new MockSlotfareApi();
    });
    runConformance(() => api, noopFlush);

    it('usageProof() rejects with 404 for a pending row and an unknown row', async () => {
      // The newest fixture row is the demo's pending charge: no proof exists
      // yet, and an unknown id never did.
      await expect(api.usageProof('u0')).rejects.toSatisfy(
        (e: unknown) => e instanceof ApiError && e.status === 404,
      );
      await expect(api.usageProof('u999')).rejects.toSatisfy(
        (e: unknown) => e instanceof ApiError && e.status === 404,
      );
    });

    it('chains() serves the single Solana row with its explorer and RPC', async () => {
      const chains = await api.chains();
      expect(chains.length).toBe(1);
      const solana = chains[0];
      expect(solana.name).toBe('Solana');
      expect(solana.deposit).not.toBe('');
      expect(solana.explorer).toContain('solana');
      expect(solana.rpc).toContain('http');
    });

    it('usageProof() of every settled fixture row verifies against that row\'s own commitment', async () => {
      const page = await api.usage();
      const settled = page.rows.filter((r) => r.batchIndex !== null);
      expect(settled.length).toBeGreaterThan(0);
      for (const row of settled) {
        const proof = await api.usageProof(row.id);
        expect(verifyUsageProof(proof, row.root!)).toBe(true);
      }
    });

    it('the served commitment agrees with the fixture settlement facts', async () => {
      const page = await api.usage();
      const row = page.rows.find((r) => r.id === 'u1');
      expect(row).toBeDefined();
      const fixture = USAGE.find((r) => r.id === 'u1')!;
      expect(fixture.commitment).not.toBeNull();
      expect(row!.batchIndex).toBe(fixture.commitment!.batchIndex);
      expect(row!.root).toBe(fixture.commitment!.root);
      expect(row!.commitTx).toBe(fixture.commitment!.commitTx);
      // And the pending row carries none of it.
      expect(page.rows.find((r) => r.id === 'u0')!.root).toBeNull();
      expect(page.rows.find((r) => r.id === 'u0')!.commitTx).toBeNull();
    });

    it('usage({key, days}) applies the server-side filters', async () => {
      const page = await api.usage({ key: '2Mv9', days: 2 });
      expect(page.rows.length).toBeGreaterThan(0);
      expect(page.rows.length).toBeLessThanOrEqual(2);
      for (const row of page.rows) {
        expect(row.keyId).toBe('2Mv9');
      }
    });

    it('usage({before}) pages to the rows strictly older than the cursor', async () => {
      // The fixture is newest-first u0..u9; before=4 keeps only u5..u9.
      const page = await api.usage({ before: 4 });
      expect(page.rows.map((r) => r.id)).toEqual(['u5', 'u6', 'u7', 'u8', 'u9']);
      // The batch reaches the oldest row, so the history is exhausted.
      expect(page.nextCursor).toBeNull();

      // A limit cuts the batch short and names the next cursor.
      const paged = await api.usage({ before: 4, limit: 2 });
      expect(paged.rows.map((r) => r.id)).toEqual(['u5', 'u6']);
      expect(paged.nextCursor).toBe(6);

      // An unknown cursor yields no rows, mirroring the server's strict cut.
      const none = await api.usage({ before: 99 });
      expect(none.rows).toEqual([]);
      expect(none.nextCursor).toBeNull();
    });

    it('listings(before) pages to the rows strictly older than the cursor', async () => {
      // The demo fixture is four rows in one batch, so no cursor is live.
      const all = await api.listings();
      expect(all.rows.length).toBe(4);
      expect(all.nextCursor).toBeNull();

      // The cursor cuts at the cursor row's position; ids are not monotonic
      // with position, only the order is.
      const afterOne = await api.listings('1');
      expect(afterOne.rows.map((l) => l.id)).toEqual(['0', '2', '8']);
      expect(afterOne.nextCursor).toBeNull();

      const afterTwo = await api.listings('2');
      expect(afterTwo.rows.map((l) => l.id)).toEqual(['8']);
      expect(afterTwo.nextCursor).toBeNull();
    });

    it('chains() carries the served rpc on the Solana row', async () => {
      const chains = await api.chains();
      const solana = chains.find((c) => c.chainKey === 0);
      expect(solana).toBeDefined();
      // The demo row serves a public cluster endpoint: explorer links and
      // cluster facts ride it, never a hardcoded host in the bundle.
      expect(solana?.rpc).toContain('http');
    });

    it('attestation() serves the demo list for the Solana watcher', async () => {
      const list = await api.attestation();
      expect(list.length).toBeGreaterThanOrEqual(1);
      // A healthy row the card can render: known, with honest lag figures.
      expect(list.some((c) => c.known)).toBe(true);
      expect(list.every((c) => typeof c.name === 'string' && c.name !== '')).toBe(true);
    });

    it('fromAttestationWire defends a partial or malformed row', () => {
      const missing = fromAttestationWire({});
      expect(missing).toEqual({
        chainKey: 0,
        name: '',
        known: false,
        attestedTip: 0,
        head: 0,
        lagBlocks: 0,
        lagSeconds: 0,
      });
      const junk: AttestationChainWire = {
        chainKey: 'one',
        name: null,
        known: 'yes',
        attestedTip: -5,
        head: Number.NaN,
        lagBlocks: Infinity,
        lagSeconds: 42,
      };
      expect(fromAttestationWire(junk)).toEqual({
        chainKey: 0,
        name: '',
        known: false,
        attestedTip: 0,
        head: 0,
        lagBlocks: 0,
        lagSeconds: 42,
      });
    });

    it('market() serves the same envelope and rows as the live route (adapter parity)', async () => {
      // Feed the mock's wire envelope through the same shared adapter the
      // live path uses; a shape drift between mock and server fails here.
      const throughAdapter = api.marketEnvelope.market.map(fromMarketListingWire);
      const live = await api.market();
      expect(live).toEqual(throughAdapter);
    });

    it('models() serves the same envelope and rows as the live route (adapter parity)', async () => {
      // Feed the mock's wire catalog through the same shared adapter the
      // live path uses; a shape drift between mock and server fails here.
      const throughAdapter = api.modelEnvelope.models.map(fromModelWire);
      const live = await api.models();
      expect(live.models).toEqual(throughAdapter);
      // The demo wire advertises no base URL, so the page-origin fallback stays.
      expect(live.baseUrl).toBe('');
    });

    it('listings() carries one shared pay-token address and six decimals', async () => {
      const list = (await api.listings()).rows;
      expect(list.length).toBeGreaterThan(0);
      const mint = list[0].tokenAddress ?? '';
      expect(mint.length).toBeGreaterThan(0);
      for (const l of list) {
        expect(l.tokenAddress).toBe(mint);
        expect(l.tokenDecimals).toBe(6);
      }
    });

    it('the mock demo rows carry the trade facts a finished trade renders', async () => {
      const list = (await api.listings()).rows;
      const sold = list.find((l) => l.state === 'sold' && l.role === 'selling');
      expect(sold).toBeDefined();
      expect(sold!.listedBlock).toBe(40201);
      expect(sold!.maturesAt).toBe(1755100000);
      expect(sold!.soldTx).toBe('MockSaleSig11111111111111111111111111111111111111');
      expect(sold!.soldBlock).toBe(40207);
      expect(sold!.soldAt).toBe('2026-08-15T11:45:00Z');
      const cancelled = list.find((l) => l.state === 'cancelled');
      expect(cancelled).toBeDefined();
      expect(cancelled!.cancelledTx).toBe('MockCancelSig11111111111111111111111111111111111');
      expect(cancelled!.cancelledBlock).toBe(40190);
      expect(cancelled!.cancelledAt).toBe('2026-08-20T08:00:00Z');
      const bought = list.find((l) => l.state === 'sold' && l.role === 'buying');
      expect(bought).toBeDefined();
      expect(bought!.soldTx).toBe('0xmocksaletx');
      // The open row keeps no sale or cancel facts.
      const open = list.find((l) => l.state === 'open');
      expect(open).toBeDefined();
      expect(open!.soldTx).toBeUndefined();
      expect(open!.cancelledTx).toBeUndefined();
    });
  });

  describe('HttpSlotfareApi (conformance via testing controller)', () => {
    let api: HttpSlotfareApi;
    let http: HttpTestingController;

    function makeFlusher(h: () => HttpTestingController): Flusher {
      return (method, path, body, status = 200) => {
        const req = h().expectOne({ method, url: BASE + path });
        req.flush(body, { status, statusText: status === 200 ? 'OK' : 'Error' });
      };
    }

    beforeEach(() => {
      TestBed.configureTestingModule({
        providers: [
          provideHttpClient(withFetch()),
          provideHttpClientTesting(),
          { provide: WalletService, useValue: makeWalletStub() },
          HttpSlotfareApi,
        ],
      });
      api = TestBed.inject(HttpSlotfareApi);
      http = TestBed.inject(HttpTestingController);
    });

    afterEach(() => http.verify());

    runConformance(() => api, makeFlusher(() => http));

    it('models() normalizes the advertised baseUrl at the wire boundary', async () => {
      // A payload carrying a base URL surfaces it on the ModelList.
      const p1 = api.models();
      http.expectOne({ method: 'GET', url: `${BASE}/models` }).flush(
        { models: [], baseUrl: 'https://wire.example/v1' },
        { status: 200, statusText: 'OK' },
      );
      expect((await p1).baseUrl).toBe('https://wire.example/v1');

      // An absent baseUrl normalizes to ''.
      const p2 = api.models();
      http.expectOne({ method: 'GET', url: `${BASE}/models` }).flush(
        { models: [] },
        { status: 200, statusText: 'OK' },
      );
      expect((await p2).baseUrl).toBe('');

      // A malformed value (a number) is ignored, not passed through.
      const p3 = api.models();
      http.expectOne({ method: 'GET', url: `${BASE}/models` }).flush(
        { models: [], baseUrl: 42 },
        { status: 200, statusText: 'OK' },
      );
      expect((await p3).baseUrl).toBe('');
    });

    it('usage(query) sends the key as the prefix filter and drops the day range', async () => {
      const p = api.usage({ key: '2Mv9', days: 2 });
      const req = http.expectOne({ method: 'GET', url: `${BASE}/usage?prefix=2Mv9` });
      req.flush({ usage: [] }, { status: 200, statusText: 'OK' });
      await p;
    });

    it('sends before= on the wire for the paged reads and round-trips nextCursor', async () => {
      const pu = api.usage({ before: 5 });
      const ru = http.expectOne({ method: 'GET', url: `${BASE}/usage?before=5` });
      ru.flush({ usage: [], nextCursor: 9 }, { status: 200, statusText: 'OK' });
      expect((await pu).nextCursor).toBe(9);

      const pl = api.listings('4');
      const rl = http.expectOne({ method: 'GET', url: `${BASE}/listings?before=4` });
      rl.flush({ listings: [] }, { status: 200, statusText: 'OK' });
      // An absent nextCursor is exhaustion, not an error.
      expect((await pl).nextCursor).toBeNull();
    });

    it('usage() normalizes wire facets: the All placeholder and duplicates are dropped', async () => {
      const p = api.usage({ page: 1, limit: 25 });
      http.expectOne({ method: 'GET', url: `${BASE}/usage?page=1&limit=25` }).flush(
        {
          usage: [],
          total: 0,
          facets: { prefixes: ['', '7Qk4', '7Qk4'], models: ['', 'a', 'b'] },
        },
        { status: 200, statusText: 'OK' },
      );
      const page = await p;
      expect(page.facets).toEqual({ prefixes: ['7Qk4'], models: ['a', 'b'] });
    });

    it('account() divides a sub-credit wire balance down to credits', async () => {
      // The mock ignores flushes, so this lives with the HTTP path only:
      // five extra micro-credits must surface as 0.0005, never as 0.
      const p = api.account();
      const body = accountBody();
      const req = http.expectOne({ method: 'GET', url: BASE + '/account' });
      req.flush(
        { ...body, spendableMicro: body.spendableMicro + 5 },
        { status: 200, statusText: 'OK' },
      );
      const acct = await p;
      expect(acct.balanceCredits).toBeCloseTo(OPENING_BALANCE + 0.0005, 5);
    });

    it('usage() maps absent or malformed commitment fields to null, never undefined', async () => {
      const p = api.usage();
      http.expectOne({ method: 'GET', url: `${BASE}/usage` }).flush(
        {
          usage: [
            { timestamp: '2026-08-22T14:22:07Z', service: 'inference', model: 'm', promptTokens: 1, cachedTokens: 0, completionTokens: 1, creditsMicro: 100, status: 'ok', keyPrefix: 'k', batchIndex: 'three', root: 42, commitTx: '' },
            { timestamp: '2026-08-22T14:21:54Z', service: 'inference', model: 'm', promptTokens: 1, cachedTokens: 0, completionTokens: 1, creditsMicro: 100, status: 'ok', keyPrefix: 'k', batchIndex: 7, root: '0xroot', commitTx: '0xcommit' },
          ],
        },
        { status: 200, statusText: 'OK' },
      );
      const page = await p;
      // Non-conforming values read as absent: null, not the junk itself.
      expect(page.rows[0].batchIndex).toBeNull();
      expect(page.rows[0].root).toBeNull();
      expect(page.rows[0].commitTx).toBeNull();
      expect(page.rows[1].batchIndex).toBe(7);
      expect(page.rows[1].root).toBe('0xroot');
      expect(page.rows[1].commitTx).toBe('0xcommit');
    });

    it('usage() classifies wire status as error and passes duration_ms through (adapter parity)', async () => {
      // The wire's error-ness lives only in `status` ('ok' | 'error'), and
      // duration_ms is null for rows written before the column existed; the
      // adapter must surface both, so the client's Metered/Error narrowing
      // and the duration cell agree with the live server.
      const pageFixture = {
        usage: [
          { timestamp: '2026-08-22T14:22:07Z', service: 'inference', model: 'qwen3.8-27b', promptTokens: 8412, cachedTokens: 6144, completionTokens: 1206, creditsMicro: 130_000, status: 'ok', keyPrefix: '7Qk4', duration_ms: 541 },
          { timestamp: '2026-08-22T14:21:54Z', service: 'inference', model: 'qwen3.8-27b', promptTokens: 2190, cachedTokens: 0, completionTokens: 884, creditsMicro: 50_000, status: 'error', keyPrefix: '2Mv9', duration_ms: 12700 },
          { timestamp: '2026-08-22T14:21:31Z', service: 'inference', model: 'qwen3.8-27b', promptTokens: 100, cachedTokens: 0, completionTokens: 50, creditsMicro: 1_000, status: 'ok', keyPrefix: '7Qk4' },
        ],
      };
      const p = api.usage();
      http.expectOne({ method: 'GET', url: `${BASE}/usage` }).flush(pageFixture, {
        status: 200,
        statusText: 'OK',
      });
      const page = await p;
      expect(page.rows.map((r) => r.id)).toEqual([
        '2026-08-22T14:22:07Z:7Qk4:qwen3.8-27b:130000',
        '2026-08-22T14:21:54Z:2Mv9:qwen3.8-27b:50000',
        '2026-08-22T14:21:31Z:7Qk4:qwen3.8-27b:1000',
      ]);
      const [ok, err, legacy] = page.rows;
      expect(ok.error).toBeNull();
      expect(ok.latencyMs).toBe(541);
      expect(err.error).toBe('error');
      expect(err.latencyMs).toBe(12700);
      // The pre-column row keeps its absent duration as null, never 0.
      expect(legacy.error).toBeNull();
      expect(legacy.latencyMs).toBeNull();
    });
  });

  // ------------------------------------------------------------------
  // b) Bearer token behaviour (HttpSlotfareApi only)
  // ------------------------------------------------------------------

  describe('bearer token', () => {
    let walletStub: WalletStub;

    beforeEach(() => {
      walletStub = makeWalletStub();
      TestBed.configureTestingModule({
        providers: [
          provideHttpClient(withFetch()),
          provideHttpClientTesting(),
          { provide: WalletService, useValue: walletStub },
          HttpSlotfareApi,
        ],
      });
    });

    afterEach(() => TestBed.inject(HttpTestingController).verify());

    it('omits Authorization header when no session exists', async () => {
      const api = TestBed.inject(HttpSlotfareApi);
      const http = TestBed.inject(HttpTestingController);
      const p = api.account();
      const req = http.expectOne({ method: 'GET', url: BASE + '/account' });
      expect(req.request.headers.get('Authorization')).toBeNull();
      req.flush(accountBody(), { status: 200, statusText: 'OK' });
      await p;
    });

    it('attaches Bearer token when a session exists', async () => {
      walletStub.token.set('tok-123');
      const api = TestBed.inject(HttpSlotfareApi);
      const http = TestBed.inject(HttpTestingController);
      const p = api.account();
      const req = http.expectOne({ method: 'GET', url: BASE + '/account' });
      expect(req.request.headers.get('Authorization')).toBe('Bearer tok-123');
      req.flush(accountBody(), { status: 200, statusText: 'OK' });
      await p;
    });
  });

  // ------------------------------------------------------------------
  // c) 401 mapping (HttpSlotfareApi only)
  // ------------------------------------------------------------------

  describe('error mapping', () => {
    let api: HttpSlotfareApi;
    let http: HttpTestingController;

    beforeEach(() => {
      TestBed.configureTestingModule({
        providers: [
          provideHttpClient(withFetch()),
          provideHttpClientTesting(),
          { provide: WalletService, useValue: makeWalletStub() },
          HttpSlotfareApi,
        ],
      });
      api = TestBed.inject(HttpSlotfareApi);
      http = TestBed.inject(HttpTestingController);
    });

    afterEach(() => http.verify());

    it('surfaces a 401 as ApiError with status 401 and code from body', async () => {
      const p = api.account();
      const req = http.expectOne({ method: 'GET', url: BASE + '/account' });
      req.flush({ code: 'unauthorized' }, { status: 401, statusText: 'Unauthorized' });

      await expect(p).rejects.toSatisfy(
        (e: unknown) =>
          e instanceof ApiError && e.status === 401 && e.code === 'unauthorized',
      );
    });

    it('does not leak HttpErrorResponse to the caller', async () => {
      const p = api.account();
      const req = http.expectOne({ method: 'GET', url: BASE + '/account' });
      req.flush({ code: 'unauthorized' }, { status: 401, statusText: 'Unauthorized' });

      let caught: unknown;
      try {
        await p;
      } catch (e) {
        caught = e;
      }
      expect(caught).toBeInstanceOf(ApiError);
    });
  });
});

describe('fromPurchaseView', () => {
  const view = { tip: 150, ceiling: 120, purchases: [] };
  const wire = (stage: number, sourceBlock = 100) => ({
    sourceTx: '0xabc',
    sourceBlock,
    attestedHeight: 0,
    ceiling: 120,
    stage,
    depositId: '1',
    token: 'mUSDC',
    amountUsd: 25,
    startedAt: '2026-08-23T23:00:00Z',
    credited: stage >= 5,
    ccTx: stage >= 5 ? '0xcc' : '',
    ccBlock: stage >= 5 ? 4242 : 0,
    creditedAt: stage >= 5 ? '2026-08-23T23:10:00Z' : '',
    credits: stage >= 5 ? 25000 : 0,
    chainKey: 1,
    chainName: 'Ethereum',
  });

  it('maps a recorded purchase with no block to pending', () => {
    const s = fromPurchaseView(wire(1, 0), view);
    expect(s).toMatchObject({ purchaseId: '0xabc', ethTx: '0xabc', state: 'pending', ethBlock: null, attestedHeight: null, builderCeiling: null });
  });

  it('maps a confirmed purchase to its block without attestation facts', () => {
    const s = fromPurchaseView(wire(2), view);
    expect(s).toMatchObject({ state: 'confirmed', ethBlock: 100, attestedHeight: null, chainTip: 150, builderCeiling: null });
  });

  it('maps an attested purchase to the attested height and the ceiling', () => {
    const s = fromPurchaseView(wire(4), view);
    expect(s).toMatchObject({ state: 'confirmed', ethBlock: 100, attestedHeight: 100, chainTip: 150, builderCeiling: 120 });
  });

  it('maps a credited purchase to settled with the Solana facts', () => {
    const s = fromPurchaseView(wire(5), view);
    expect(s).toMatchObject({ state: 'settled', ccTx: '0xcc', ccBlock: 4242, credits: 25000, amountUsd: 25, token: 'mUSDC', updatedAt: '2026-08-23T23:10:00Z' });
  });

  it('maps the source chain key and name onto the funding status', () => {
    const s = fromPurchaseView(wire(1), view);
    expect(s.chainKey).toBe(1);
    expect(s.chainName).toBe('Ethereum');
  });

  it('ignores a swap leg the Solana path never sends', () => {
    const s = fromPurchaseView({ ...wire(2), swap: { tokenIn: 'x' } } as never, view);
    expect((s as { swap?: unknown }).swap).toBeUndefined();
  });
});


describe('fromListingWire', () => {
  it('passes the finished-trade facts straight through the adapter', () => {
    const w = {
      listingId: '7',
      credits: 10_000,
      payTokenSymbol: 'mUSD1',
      price: 10_000_000_000,
      matured: true,
      state: 'sold' as const,
      role: 'selling' as const,
      listedTx: '0xlisttx7',
      listedAt: '2026-08-15T09:30:00Z',
      listedBlock: 40201,
      maturesAt: 1755100000,
      soldTx: '0xsaletx7',
      soldBlock: 40207,
      soldAt: '2026-08-15T11:45:00Z',
    };
    const l = fromListingWire(w);
    expect(l.listedBlock).toBe(40201);
    expect(l.maturesAt).toBe(1755100000);
    expect(l.soldTx).toBe('0xsaletx7');
    expect(l.soldBlock).toBe(40207);
    expect(l.soldAt).toBe('2026-08-15T11:45:00Z');
  });

  it('treats a zero or absent block as unknown (Go omitempty drops zeros)', () => {
    const open = fromListingWire({
      listingId: '1',
      credits: 5_000,
      payTokenSymbol: 'mUSD1',
      price: 500,
      matured: false,
      state: 'open',
      role: 'selling',
      listedTx: '0xlisttx1',
      listedAt: '2026-08-22T20:00:00Z',
    });
    expect(open.listedBlock).toBeUndefined();
    expect(open.soldTx).toBeUndefined();
    expect(open.cancelledTx).toBeUndefined();

    const zeroed = fromListingWire({
      listingId: '2',
      credits: 5_000,
      payTokenSymbol: 'mUSD1',
      price: 500,
      matured: true,
      state: 'cancelled',
      role: 'selling',
      listedTx: '0xlisttx2',
      listedAt: '2026-08-20T08:00:00Z',
      cancelledBlock: 0,
    });
    expect(zeroed.cancelledBlock).toBeUndefined();
    expect(zeroed.cancelledAt).toBeUndefined();
  });
});

describe('fromChainWire', () => {
  it('passes every field through', () => {
    const c: ChainInfo = fromChainWire({
      chainKey: 3,
      name: 'Ethereum',
      chainId: 1,
      deposit: '0xdep-main',
      explorer: 'https://eth.example.com',
    });
    expect(c).toEqual({
      chainKey: 3,
      name: 'Ethereum',
      chainId: 1,
      deposit: '0xdep-main',
      explorer: 'https://eth.example.com',
    });
  });

  it('passes the optional public RPC endpoint through when present', () => {
    const c = fromChainWire({ chainKey: 0, name: 'Solana', rpc: 'https://api.devnet.solana.com' });
    expect(c.rpc).toBe('https://api.devnet.solana.com');
    expect(fromChainWire({ chainKey: 0, name: 'Solana' }).rpc).toBeUndefined();
  });

  it('defaults missing fields so a partial row never renders undefined', () => {
    const c = fromChainWire({});
    expect(c.chainKey).toBe(0);
    expect(c.name).toBe('');
    expect(c.chainId).toBe(0);
    expect(c.deposit).toBe('');
    expect(c.explorer).toBe('');
  });

});

describe('fromModelWire', () => {
  it('passes a full row through unchanged', () => {
    const m = fromModelWire({
      id: 'llama-3.2-3b',
      name: 'Llama 3.2 3B',
      pricing: { uncachedInput: 10, cachedInput: 1, output: 40 },
    });
    expect(m).toEqual({
      id: 'llama-3.2-3b',
      name: 'Llama 3.2 3B',
      pricing: { uncachedInput: 10, cachedInput: 1, output: 40 },
    });
  });

  it('defaults a missing or non-numeric pricing field to 0', () => {
    const missing = fromModelWire({ id: 'a', name: 'A', pricing: { uncachedInput: 5 } });
    expect(missing.pricing.cachedInput).toBe(0);
    expect(missing.pricing.output).toBe(0);
    const notNumeric = fromModelWire({ id: 'b', name: 'B', pricing: { uncachedInput: 'oops', cachedInput: null, output: 2 } });
    expect(notNumeric.pricing.uncachedInput).toBe(0);
    expect(notNumeric.pricing.cachedInput).toBe(0);
    expect(notNumeric.pricing.output).toBe(2);
    const noPricing = fromModelWire({ id: 'c', name: 'C' });
    expect(noPricing.pricing).toEqual({ uncachedInput: 0, cachedInput: 0, output: 0 });
  });

  it('leaves a missing id as an empty string for the list reader to drop', () => {
    const m = fromModelWire({ name: 'No id', pricing: { uncachedInput: 1, cachedInput: 0, output: 1 } });
    expect(m.id).toBe('');
    expect(m.name).toBe('No id');
  });

  it('maps a full-metadata row onto every presentation field', () => {
    const m = fromModelWire({
      id: 'm-1',
      name: 'M',
      type: 'embeddings',
      vendor: 'Jina',
      contextLength: 524288,
      servingPath: 'fast',
      description: 'A demo embedding model.',
      parameters: '8B',
      capabilities: ['vision', 'function-calling'],
      moe: true,
      pricing: { uncachedInput: 5 },
    });
    expect(m).toEqual({
      id: 'm-1',
      name: 'M',
      kind: 'embeddings',
      vendor: 'Jina',
      contextLength: 524288,
      servingPath: 'fast',
      description: 'A demo embedding model.',
      parameters: '8B',
      capabilities: ['vision', 'function-calling'],
      moe: true,
      pricing: { uncachedInput: 5, cachedInput: 0, output: 0 },
    });
  });

  it('passes a boolean moe through and omits an absent or non-boolean one', () => {
    const yes = fromModelWire({ id: 'a', name: 'A', moe: true });
    expect(yes.moe).toBe(true);
    const no = fromModelWire({ id: 'b', name: 'B', moe: false });
    expect(no.moe).toBe(false);
    const junk = fromModelWire({ id: 'c', name: 'C', moe: 'yes' });
    expect('moe' in junk).toBe(false);
    const absent = fromModelWire({ id: 'd', name: 'D' });
    expect('moe' in absent).toBe(false);
  });

  it('accepts the six capability slugs and still drops unknown ones', () => {
    const all = fromModelWire({
      id: 'e',
      name: 'E',
      capabilities: ['text', 'image', 'video', 'audio', 'vision', 'function-calling', 'sql'],
    });
    expect(all.capabilities).toEqual([
      'text',
      'image',
      'video',
      'audio',
      'vision',
      'function-calling',
    ]);
  });

  it('maps malformed optional metadata to absent keys, never invented values', () => {
    const m = fromModelWire({
      id: 'm',
      name: 'M',
      vendor: 42,
      contextLength: '32768',
      type: 'audio',
      servingPath: 'turbo',
      description: '',
      parameters: null,
      capabilities: [3, 'sql', null],
    });
    expect(m).toEqual({
      id: 'm',
      name: 'M',
      pricing: { uncachedInput: 0, cachedInput: 0, output: 0 },
    });
    expect('capabilities' in m).toBe(false);
    // The one well-formed capability in a junk array survives.
    const partial = fromModelWire({ id: 'p', name: 'P', capabilities: [null, 'vision'] });
    expect(partial.capabilities).toEqual(['vision']);
  });
});
