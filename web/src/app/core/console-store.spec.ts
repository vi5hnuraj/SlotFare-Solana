import { TestBed } from '@angular/core/testing';
import { computed, signal } from '@angular/core';
import { ConsoleStore } from './console-store';
import {
  Account, ApiError, AttestChain, ChainInfo,
  CreatedKey, DepositTarget, FundingStatus, Key,
  ListingPage, MarketConfirmBody, MarketPrepareBody, ModelList,
  Nonce, PreparedMarketTx, SLOTFARE_API, SlotfareApi,
  UsagePage, UsageProof, UsageQuery, UsageRowT,
  VerifyResult,
} from './api';
import { MockSlotfareApi } from './api';
import { ConsoleSession, Eip1193LikeProvider, WalletService } from './wallet';
import { Listing, MarketListing, Model, Wallet } from './models';
import { SellService } from './sell';
import { BuyService } from './buy';
import { DepositService } from './deposit';
import { KEYS, OPENING_BALANCE, OPENING_LOCKED, USAGE, WALLET_ADDRESS } from './mock-data';
import { creditText1, usd } from './pricing';
import { environment } from '../../environments/environment';

/** Let the loader promises settle after `TestBed.flushEffects()`. */
const tick = (): Promise<void> => new Promise((r) => setTimeout(r));

/** Fake wallet: the session signals drive the store's load effect. */
function makeFakeWallet(): WalletService {
  const address = signal<string | null>(null);
  const chainId = signal<string | null>(null);
  const connecting = signal(false);
  const error = signal<string | null>(null);
  const accountId = signal<string | null>(null);
  const token = signal<string | null>(null);
  const restoredRdns = signal<string | null>(null);
  const restoredWallet = signal<Wallet | null>(null);
  const session = computed<ConsoleSession | null>(() => {
    const t = token();
    if (t === null) return null;
    const a = address();
    const id = accountId();
    if (a === null || id === null) return null;
    return { address: a, accountId: id, token: t };
  });
  // The real service has private members, so the structural match is
  // asserted through this cast.
  const fake = {
    providers: signal<Wallet[]>([]),
    address,
    chainId,
    connecting,
    error,
    accountId,
    token,
    session,
    restoredRdns,
    restoredWallet,
    provider: null as unknown as Eip1193LikeProvider | null,
    startSessionCalls: 0,
    declineOnStartSession: false,
    connect: () => undefined,
    startSession: () => {
      fake.startSessionCalls += 1;
      if (fake.declineOnStartSession) {
        error.set('Signature rejected');
      }
    },
    dismissError: () => {
      error.set(null);
    },
    dropSession: () => {
      token.set(null);
      accountId.set(null);
      // Keep the connection (address, provider) for an in-place re-sign.
    },
    clearSession: () => {
      token.set(null);
      accountId.set(null);
      address.set(null);
      error.set(null);
      restoredRdns.set(null);
      restoredWallet.set(null);
    },
  };
  return fake as unknown as WalletService;
}

/** A helper to build a FundingStatus with sensible defaults. */
/** A helper to build a FundingStatus with sensible defaults.
 *
 *  The default is a legacy imported row (chain key 1, source chain Ethereum,
 *  the three-step proof anatomy). Chain 0 is Solana, the only chain this
 *  deployment writes to; tests that exercise it set it explicitly. */
function makeFunding(overrides: Partial<FundingStatus> = {}): FundingStatus {
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

/** A helper to build the local listing row a just-sent sell announces:
 *  the create call returns it fully shaped, so the store stores it as is. */
function makeNewListingRow(): Listing {
  return {
    id: '0xnewtx',
    listedTx: '0xnewtx',
    state: 'open',
    role: 'selling',
    credits: 5_000,
    price: 500_000_000,
    token: 'mUSD1',
    tokenAddress: 'DemoUSDCmint11111111111111111111111111111111',
    tokenDecimals: 6,
    matured: true,
    createdAt: '2026-08-22T20:00:00Z',
  };
}

/** A helper to build a wire usage row with a stable id. */
function makeUsageRowT(id: string): UsageRowT {
  return {
    id,
    occurredAt: '2026-08-25T14:00:00Z',
    keyId: '7Qk4',
    model: 'llama-3.2-3b',
    promptTokens: 100,
    cachedTokens: 0,
    completionTokens: 50,
    credits: 10_000,
    status: 'ok',
    latencyMs: 10,
    error: null,
    batchIndex: null,
    root: null,
    commitTx: null,
  };
}

/** unix seconds of a timestamp, 0 when unparseable (mirrors the mock). */
function secOf(ts: string): number {
  const t = Date.parse(ts);
  return Number.isNaN(t) ? 0 : Math.floor(t / 1000);
}

/** A helper to build a finished market listing with a numeric id. */
function makeListing(id: string, role: 'selling' | 'buying'): Listing {
  return {
    id,
    listedTx: `0x${id}`,
    state: 'sold',
    role,
    credits: 5_000,
    price: 500,
    token: 'mUSD1',
    matured: true,
    createdAt: '2026-08-23T00:00:00Z',
  };
}

/** Fake API: mutable canned data plus call counts and one-shot failures. */
class FakeApi implements SlotfareApi {
  accountCalls = 0;
  keysCalls = 0;
  usageCalls = 0;
  fundingListCalls = 0;
  listingsCalls = 0;
  /** The last UsageQuery passed to usage(), for assertions. */
  lastUsageQuery: UsageQuery | null = null;
  /** When set, the next call to any method rejects with it once. */
  failNext: ApiError | null = null;
  /**
   * A persistent failure applied to every read while set: the "errors never
   * stop the loop" tests. Cleared by the test, not consumed on use.
   */
  alwaysFail: ApiError | null = null;
  /** The next fundingList() read rejects with this once; other reads are
   *  untouched, so a tick can fail just the funding figure. */
  fundingFailNext: ApiError | null = null;
  /** Mutable list returned by fundingList(). */
  fundingListData: FundingStatus[] = [];
  /** Mutable list returned by listings(). */
  listingsData: Listing[] = [];
  /** Mutable rows served by usage(), newest-first, like the fixture. */
  usageRows: UsageRowT[] = USAGE.map((r, i) => ({
    id: `u${i}`,
    occurredAt: r.time,
    keyId: r.key,
    model: r.model,
    promptTokens: r.prompt,
    cachedTokens: r.cached,
    completionTokens: r.completion,
    credits: Math.round(r.credits * 10_000),
    status: r.result,
    latencyMs: r.latencyMs,
    error: r.error,
    batchIndex: r.commitment?.batchIndex ?? null,
    root: r.commitment?.root ?? null,
    commitTx: r.commitment?.commitTx ?? null,
  }));
  /** Page sizes for the paged reads; 0 serves the whole set in one batch. */
  usagePageSize = 0;
  listingsPageSize = 0;
  /** Mutable balance returned by account(); tests can change it between polls. */
  accountBalance: number = OPENING_BALANCE;
  /** Mutable locked figure returned by account(). */
  accountLocked: number = OPENING_LOCKED;

  private readonly keysList: Key[] = KEYS.map((k) => ({
    id: k.id,
    label: k.label,
    createdAt: k.created,
    lastUsedAt: k.lastUsedAt,
    requestCount: k.requests,
    revoked: k.revoked,
  }));
  private counter = 0;

  private failOr<T>(fn: () => T): T {
    if (this.failNext !== null) {
      const e = this.failNext;
      this.failNext = null;
      throw e;
    }
    if (this.alwaysFail !== null) {
      throw this.alwaysFail;
    }
    return fn();
  }

  async nonce(_address: string): Promise<Nonce> {
    return {
      nonce: 'n',
      statement: 's',
      issuedAt: '2026-08-22T20:00:00Z',
      expiresAt: '2026-08-22T20:05:00Z',
    };
  }

  /** The inference base URL the account read advertises ('' by default;
   *  the live wire carries none, so this is the fallback path). */
  accountBaseUrl = '';
  /** The inference base URL the models payload advertises ('' by default,
   *  so the page-origin fallback stands unless a test sets it). */
  modelBaseUrl = '';

  async verify(
    _address: string,
    _nonce: string,
    _signature: string,
  ): Promise<VerifyResult> {
    return {
      accountId: '0xmockacct',
      token: 'mock-token',
      expiresAt: '2027-08-22T20:00:00Z',
    };
  }

  async account(): Promise<Account> {
    this.accountCalls += 1;
    return this.failOr(() => ({
      accountId: '0xmockacct',
      address: WALLET_ADDRESS,
      balanceCredits: this.accountBalance,
      lockedCredits: this.accountLocked,
      totalSpentCredits: 320_000,
      createdAt: '2026-06-14',
      baseUrl: this.accountBaseUrl,
    }));
  }

  async keys(): Promise<Key[]> {
    this.keysCalls += 1;
    return this.failOr(() => this.keysList.map((k) => ({ ...k })));
  }

  async createKey(label: string): Promise<CreatedKey> {
    this.counter += 1;
    const key: Key = this.failOr(() => {
      const k: Key = {
        id: 'mock' + this.counter.toString().padStart(4, '0'),
        label,
        createdAt: '2026-08-22',
        lastUsedAt: null,
        requestCount: 0,
        revoked: false,
      };
      this.keysList.unshift(k);
      return k;
    });
    return { key: { ...key }, plaintext: 'slf_live_mock' };
  }

  async revokeKey(id: string): Promise<void> {
    this.failOr(() => {
      const idx = this.keysList.findIndex((k) => k.id === id);
      if (idx === -1) {
        throw new ApiError(404, 'not_found', `Key ${id} not found`);
      }
      this.keysList[idx] = { ...this.keysList[idx], revoked: true };
    });
  }

  async usage(query?: UsageQuery): Promise<UsagePage> {
    this.usageCalls += 1;
    this.lastUsageQuery = query ?? null;
    return this.failOr(() => {
      let rows: UsageRowT[] = this.usageRows.map((r) => ({ ...r }));
      // The account's distinct filter values are independent of the active
      // filters, so they come from the full set before any narrowing.
      const facets = {
        prefixes: [...new Set(rows.map((r) => r.keyId))],
        models: [...new Set(rows.map((r) => r.model))],
      };
      if (query?.key && query.key !== 'all') {
        rows = rows.filter((r) =>
          query.key === 'errors' ? r.error !== null : r.keyId === query.key,
        );
      }
      if (query?.model) {
        rows = rows.filter((r) => r.model === query.model);
      }
      if (query?.from !== undefined) {
        rows = rows.filter((r) => secOf(r.occurredAt) >= query.from!);
      }
      if (query?.to !== undefined) {
        rows = rows.filter((r) => secOf(r.occurredAt) <= query.to!);
      }
      if (query?.errors) {
        rows = rows.filter((r) => r.error !== null);
      }
      if (query?.search) {
        const q = query.search.trim().toLowerCase();
        if (q !== '') {
          rows = rows.filter((r) =>
            [r.occurredAt, r.keyId, r.model, r.status, r.error ?? '']
              .join(' ')
              .toLowerCase()
              .includes(q),
          );
        }
      }
      const total = rows.length;
      // The numbered read (?page) serves one page of the filtered set and
      // reports total + facets; no cursor rides it.
      if (query?.page !== undefined) {
        const cap = query.limit ?? 25;
        rows = rows.slice((query.page - 1) * cap, query.page * cap);
        return { rows, nextCursor: null, total, facets };
      }
      // The cursor pages the filtered set (the server applies `before` after
      // its own row selection); the page size bounds the batch.
      const base = rows;
      if (query?.before !== undefined) {
        const idx = base.findIndex((r) => Number(r.id.slice(1)) === query.before);
        rows = idx === -1 ? [] : base.slice(idx + 1);
      }
      if (this.usagePageSize > 0) rows = rows.slice(0, this.usagePageSize);
      const last = rows[rows.length - 1];
      const tail = base[base.length - 1];
      const nextCursor =
        last !== undefined && tail !== undefined && last.id !== tail.id
          ? Number(last.id.slice(1))
          : null;
      return { rows, nextCursor };
    });
  }

  async funding(purchaseId: string): Promise<FundingStatus> {
    return makeFunding({ purchaseId, state: 'settled', ethBlock: 100, attestedHeight: 100, builderCeiling: 100, ccTx: '0xabc', ccBlock: 50, credits: OPENING_BALANCE });
  }

  async fundingList(): Promise<FundingStatus[]> {
    this.fundingListCalls += 1;
    if (this.fundingFailNext !== null) {
      const e = this.fundingFailNext;
      this.fundingFailNext = null;
      throw e;
    }
    return this.failOr(() => this.fundingListData.map((s) => ({ ...s })));
  }

  async listings(before?: string): Promise<ListingPage> {
    this.listingsCalls += 1;
    return this.failOr(() => {
      const p = this.page(this.listingsData, before === undefined ? undefined : Number(before), (l) => Number(l.id), this.listingsPageSize);
      return { rows: p.rows, nextCursor: p.nextCursor === null ? null : String(p.nextCursor) };
    });
  }

  /** Page a newest-first fixture array by a before-id cursor, mirroring the
   *  server: rows strictly older than the cursor's row, and nextCursor the
   *  last returned row's key unless it is already the oldest of the set. */
  private page<T>(all: T[], before: number | undefined, key: (t: T) => number, pageSize = 0): { rows: T[]; nextCursor: number | null } {
    let rows = all.map((r) => ({ ...r }));
    if (before !== undefined) {
      const idx = rows.findIndex((r) => key(r) === before);
      rows = idx === -1 ? [] : rows.slice(idx + 1);
    }
    if (pageSize > 0) rows = rows.slice(0, pageSize);
    const last = rows[rows.length - 1];
    const tail = all[all.length - 1];
    const nextCursor =
      last !== undefined && tail !== undefined && Number.isFinite(key(last)) && key(last) !== key(tail)
        ? key(last)
        : null;
    return { rows, nextCursor };
  }

  marketCalls = 0;
  /** Mutable list returned by market(). */
  marketData: MarketListing[] = [];

  async market(): Promise<MarketListing[]> {
    this.marketCalls += 1;
    return this.failOr(() => this.marketData.map((l) => ({ ...l })));
  }

  chainsCalls = 0;
  /** Mutable list returned by chains(). */
  chainsData: ChainInfo[] = [];

  async depositTarget(): Promise<DepositTarget> {
    throw new Error('not implemented');
  }
  async createListing(_body: { credits: number; price: number }): Promise<Listing> {
    throw new Error('not implemented');
  }
  async cancelListing(_id: string): Promise<Listing> {
    throw new Error('not implemented');
  }
  async payListing(_id: string, _signature: string): Promise<Listing> {
    throw new Error('not implemented');
  }
  async prepareMarket(_body: MarketPrepareBody): Promise<PreparedMarketTx> {
    throw new Error('not implemented');
  }
  async confirmMarket(_body: MarketConfirmBody): Promise<Listing> {
    throw new Error('not implemented');
  }

  async chains(): Promise<ChainInfo[]> {
    this.chainsCalls += 1;
    return this.failOr(() => this.chainsData.map((c) => ({ ...c })));
  }

  attestationCalls = 0;
  /** Mutable list returned by attestation(). */
  attestationData: AttestChain[] = [];
  /** When set, the next attestation() read rejects with it once. */
  attestationFailNext: ApiError | null = null;

  async attestation(): Promise<AttestChain[]> {
    this.attestationCalls += 1;
    if (this.attestationFailNext !== null) {
      const e = this.attestationFailNext;
      this.attestationFailNext = null;
      throw e;
    }
    return this.failOr(() => this.attestationData.map((c) => ({ ...c })));
  }

  /** The demo catalog; none of it beats the `RATE` fallback's per-million
   *  figures, so the store's token estimates stay on the cheapest model. */
  readonly modelData: Model[] = [
    { id: 'mistral-nemotron', name: 'Mistral Nemotron', pricing: { uncachedInput: 20, cachedInput: 2, output: 30 } },
    { id: 'diffusiongemma-26b', name: 'Google DiffusionGemma 26B A4B', pricing: { uncachedInput: 50, cachedInput: 5, output: 250 } },
    { id: 'llama-3.2-3b', name: 'Llama 3.2 3B', pricing: { uncachedInput: 10, cachedInput: 1, output: 40 } },
    { id: 'qwen3-8-27b', name: 'Qwen3 27B', pricing: { uncachedInput: 100, cachedInput: 10, output: 300 } },
  ];

  async models(): Promise<ModelList> {
    return this.failOr(() => ({
      models: this.modelData.map((m) => ({ ...m })),
      baseUrl: this.modelBaseUrl,
    }));
  }

  // ---- inclusion proof (EP-027-T05) ------------------------------------
  usageProofCalls = 0;
  /** When set, the next usageProof() rejects with this once. */
  usageProofFailNext: ApiError | null = null;
  /** When true, the next served proof's sibling path is corrupted once.
   *  The walk then lands on the wrong root: the failed state. */
  tamperNextProof = false;
  /** The mock serves the fixture's commitment tree; the fake's usage rows
   *  are that same fixture, so the ids line up. */
  private readonly proofSource = new MockSlotfareApi();

  async usageProof(rowId: string): Promise<UsageProof> {
    this.usageProofCalls += 1;
    if (this.usageProofFailNext !== null) {
      const e = this.usageProofFailNext;
      this.usageProofFailNext = null;
      throw e;
    }
    const proof = await this.proofSource.usageProof(rowId);
    if (this.tamperNextProof) {
      this.tamperNextProof = false;
      const p = proof.path;
      return {
        ...proof,
        path: p.length > 1 ? [p[p.length - 1], ...p.slice(0, -1)] : p,
      };
    }
    return proof;
  }
}

/** Offline EventSource stand-in: jsdom has no EventSource, so the suite
 *  never touches the network. Records instances and lets tests drive
 *  open/fail/emit by hand. */
class FakeEventSource {
  static instances: FakeEventSource[] = [];
  url: string;
  readyState = 0; // CONNECTING
  onopen: (() => void) | null = null;
  onerror: (() => void) | null = null;
  private readonly listeners = new Map<string, ((ev: Event) => void)[]>();

  constructor(url: string) {
    this.url = url;
    FakeEventSource.instances.push(this);
  }

  addEventListener(type: string, handler: (ev: Event) => void): void {
    const list = this.listeners.get(type) ?? [];
    list.push(handler);
    this.listeners.set(type, list);
  }

  removeEventListener(_type: string, _handler: (ev: Event) => void): void {}

  close(): void {
    this.readyState = 2; // CLOSED
  }

  /** Fire onopen as if the socket opened. */
  open(): void {
    this.readyState = 1; // OPEN
    this.onopen?.();
  }

  /** Close and fire onerror as if the socket dropped. */
  fail(): void {
    this.readyState = 2;
    this.onerror?.();
  }

  /** Emit a named SSE event with a JSON payload. */
  emit(type: string, obj: unknown): void {
    const event = new MessageEvent(type, { data: JSON.stringify(obj) });
    for (const h of this.listeners.get(type) ?? []) h(event);
  }
}

describe('ConsoleStore', () => {
  let store: ConsoleStore;
  let fakeApi: FakeApi;
  let fakeWallet: WalletService;
  /** SellService stand-in: the wallet path is covered in sell.spec.ts; the
   *  store only needs to see cancel succeed or fail. */
  let fakeSell: {
    step: ReturnType<typeof signal<string>>;
    error: ReturnType<typeof signal<string | null>>;
    txHash: ReturnType<typeof signal<string | null>>;
    cancelCalls: string[];
    failNext: Error | null;
    /** When set, the next cancel never settles: the prompt stays open. */
    hangNext: boolean;
  };
  /** BuyService stand-in, same shape as the sell one. */
  let fakeBuy: {
    step: ReturnType<typeof signal<string>>;
    error: ReturnType<typeof signal<string | null>>;
    txHash: ReturnType<typeof signal<string | null>>;
    listingGone: ReturnType<typeof signal<boolean>>;
  };
  /** DepositService stand-in: the store only reaches for its error card. */
  let fakeDeposit: {
    error: ReturnType<typeof signal<string | null>>;
    busy: ReturnType<typeof signal<boolean>>;
  };

  // The stream gate is environment.useMock (true in the demo env), so flip
  // it for this file only; vitest isolates module graphs per file.
  const useMockBefore = environment.useMock;

  beforeAll(() => {
    vi.stubGlobal('EventSource', FakeEventSource);
    environment.useMock = false;
  });

  afterAll(() => {
    vi.unstubAllGlobals();
    environment.useMock = useMockBefore;
  });

  beforeEach(() => {
    FakeEventSource.instances = [];
    fakeApi = new FakeApi();
    fakeWallet = makeFakeWallet();
    fakeSell = {
      step: signal('idle'),
      error: signal<string | null>(null),
      txHash: signal<string | null>(null),
      cancelCalls: [],
      failNext: null,
      hangNext: false,
    };
    fakeBuy = {
      step: signal('idle'),
      error: signal<string | null>(null),
      txHash: signal<string | null>(null),
      listingGone: signal(false),
    };
    fakeDeposit = {
      error: signal<string | null>(null),
      busy: signal(false),
    };
    TestBed.configureTestingModule({
      providers: [
        { provide: SLOTFARE_API, useValue: fakeApi },
        { provide: WalletService, useValue: fakeWallet },
        {
          provide: SellService,
          useValue: {
            step: fakeSell.step,
            error: fakeSell.error,
            txHash: fakeSell.txHash,
            busy: computed(() => false),
            list: async (_c: number, _s: string, _p: number): Promise<string> => '0xlisttx',
            cancel: (id: string): Promise<string> => {
              fakeSell.cancelCalls.push(id);
              if (fakeSell.failNext !== null) {
                const e = fakeSell.failNext;
                fakeSell.failNext = null;
                return Promise.reject(e);
              }
              if (fakeSell.hangNext) {
                fakeSell.hangNext = false;
                fakeSell.step.set('cancelling');
                return new Promise(() => {});
              }
              return Promise.resolve('0xcanceltx');
            },
            dismissError: () => {
              fakeSell.error.set(null);
              fakeSell.step.set('idle');
              fakeSell.txHash.set(null);
            },
          },
        },
        {
          provide: BuyService,
          useValue: {
            step: fakeBuy.step,
            error: fakeBuy.error,
            txHash: fakeBuy.txHash,
            listingGone: fakeBuy.listingGone,
            busy: computed(() => false),
            buy: async (_l: { id: string }): Promise<string> => '0xbuytx',
            dismissError: () => {
              fakeBuy.error.set(null);
              fakeBuy.step.set('idle');
              fakeBuy.txHash.set(null);
              fakeBuy.listingGone.set(false);
            },
          },
        },
        {
          provide: DepositService,
          useValue: {
            busy: fakeDeposit.busy,
            error: fakeDeposit.error,
            target: signal(null),
            loadTarget: async (): Promise<void> => {},
            deposit: async (_amountUsd: number): Promise<string> => '0xdeposit',
            dismissError: () => {
              fakeDeposit.error.set(null);
            },
          },
        },
        ConsoleStore,
      ],
    });
    store = TestBed.inject(ConsoleStore);
    TestBed.flushEffects();
  });

  afterEach(() => {
    store.stopPolling();
    store.stopListingConfirm();
    store.stopStream();
  });

  /** Establish a session and let the effect-driven loads settle. */
  async function establishSession(): Promise<void> {
    fakeWallet.address.set(WALLET_ADDRESS);
    fakeWallet.accountId.set('0xmockacct');
    fakeWallet.token.set('mock-token');
    TestBed.flushEffects();
    await tick();
  }

  /**
   * Establish a session under fake timers. `establishSession()`'s real
   * `tick()` would never fire while timers are faked, so settle instead by
   * flushing the queue. The shared loop's interval is created on the fake
   * clock, so advancing it drives ticks deterministically.
   */
  async function establishSessionFake(): Promise<void> {
    fakeWallet.address.set(WALLET_ADDRESS);
    fakeWallet.accountId.set('0xmockacct');
    fakeWallet.token.set('mock-token');
    TestBed.flushEffects();
    await vi.advanceTimersByTimeAsync(0);
  }

  it('starts disconnected with empty state, not fixture state', () => {
    expect(store.connected()).toBe(false);
    expect(store.balance()).toBe(0);
    expect(store.locked()).toBe(0);
    expect(store.balanceLoading()).toBe(false);
    expect(store.balanceError()).toBeNull();
    expect(store.keys()).toEqual([]);
    expect(store.usage()).toEqual([]);
    expect(store.listings()).toEqual([]);
    expect(fakeApi.accountCalls).toBe(0);
    expect(store.address()).toBe(WALLET_ADDRESS);
  });

  it('is not signed in on a bare wallet connection, only on a verified session', async () => {
    store.connect({ name: 'W', rdns: 'io.test', initial: 'W' });
    expect(store.connected()).toBe(true);
    expect(store.signedIn()).toBe(false);
    await establishSession();
    expect(store.signedIn()).toBe(true);
    store.disconnect();
    expect(store.signedIn()).toBe(false);
  });

  it('loads a session that matches pricing.ts exactly', async () => {
    await establishSession();

    expect(store.balance()).toBe(OPENING_BALANCE);
    expect(store.balanceUsd()).toBe(usd(OPENING_BALANCE));
    expect(store.balanceText()).toBe(creditText1(OPENING_BALANCE));
    expect(store.locked()).toBe(OPENING_LOCKED);
    expect(store.lockedText()).toBe(creditText1(OPENING_LOCKED));

    // The model catalog loaded with the session: four rows, the first
    // preselected, and llama the cheapest by uncached input price.
    expect(store.models().length).toBe(4);
    expect(store.selectedModel()?.id).toBe('mistral-nemotron');
    expect(store.cheapestModel()?.id).toBe('llama-3.2-3b');
    expect(store.modelsError()).toBeNull();

    expect(store.keys().length).toBe(4);
    expect(store.keys().find((k) => k.id === '7Qk4')?.requests).toBe(184_203);

    expect(store.usage().length).toBe(10);
    expect(store.usage()[0].credits).toBe(12.6444);
    const expectedCredits = USAGE.reduce((acc, r) => acc + r.credits, 0);
    expect(store.usageTotals().credits).toBeCloseTo(expectedCredits, 4);

    expect(store.balanceLoading()).toBe(false);
    expect(store.keysLoading()).toBe(false);
    expect(store.usageLoading()).toBe(false);
    expect(store.balanceError()).toBeNull();
    expect(store.keysError()).toBeNull();
    expect(store.usageError()).toBeNull();
  });

  it('leaves the previous value in place on a 500 and sets the error', async () => {
    await establishSession();
    expect(store.balance()).toBe(OPENING_BALANCE);

    fakeApi.failNext = new ApiError(500, 'internal', 'boom');
    await store.loadAccount();
    expect(store.balance()).toBe(OPENING_BALANCE);
    expect(store.balanceError()).not.toBeNull();
    expect(store.balanceLoading()).toBe(false);

    const before = store.keys();
    fakeApi.failNext = new ApiError(500, 'internal', 'boom');
    await store.loadKeys();
    expect(store.keys()).toEqual(before);
    expect(store.keysError()).not.toBeNull();
  });

  it('keeps every initial-load gate down through a refresh cycle', async () => {
    await establishSession();

    // While each refresh is in flight the transient loading flag is up,
    // but the paired initial gate stays down: the data already exists.
    void store.loadAccount();
    expect(store.balanceLoading()).toBe(true);
    expect(store.balanceInitial()).toBe(false);

    void store.loadListings();
    expect(store.listingsLoading()).toBe(true);
    expect(store.listingsInitial()).toBe(false);

    // Market is read on demand, not by the session effect: settle the
    // first read so data exists, then a refresh must keep the gate down.
    await store.loadMarket();
    void store.loadMarket();
    expect(store.marketLoading()).toBe(true);
    expect(store.marketInitial()).toBe(false);;
    void store.loadKeys();
    expect(store.keysLoading()).toBe(true);
    expect(store.keysInitial()).toBe(false);

    void store.loadUsage();
    expect(store.usageLoading()).toBe(true);
    expect(store.usageInitial()).toBe(false);

    void store.loadChains();
    expect(store.chainsLoading()).toBe(true);
    expect(store.chainsInitial()).toBe(false);

    void store.loadModels();
    expect(store.modelsLoading()).toBe(true);
    expect(store.modelsInitial()).toBe(false);

    // Funding: the first poll settled during the session, so its gate is
    // down even though the flag has been released again.
    expect(store.fundingInitial()).toBe(false);
    await tick();
  });

  it('keeps stale values and the gate down when a refresh fails', async () => {
    await establishSession();
    fakeApi.accountBalance = OPENING_BALANCE + 1;
    fakeApi.failNext = new ApiError(500, 'internal', 'boom');
    await store.loadAccount();
    expect(store.balance()).toBe(OPENING_BALANCE);
    expect(store.balanceError()).not.toBeNull();
    expect(store.balanceInitial()).toBe(false);
  });

  it('re-arms the initial gates when the session drops', async () => {
    await establishSession();
    fakeWallet.clearSession();
    TestBed.flushEffects();
    void store.loadAccount();
    expect(store.balanceLoading()).toBe(true);
    expect(store.balanceInitial()).toBe(true);
    await tick();
    expect(store.balanceInitial()).toBe(false);
  });

  it('loads a numbered usage page and reports the matching total and facets', async () => {
    fakeApi.usageRows = [
      makeUsageRowT('u0'),
      makeUsageRowT('u1'),
      makeUsageRowT('u2'),
      makeUsageRowT('u3'),
    ];
    await establishSession();
    // Page one of the paged read: the whole set (<= 25) plus the matching
    // total and the account's distinct key prefixes / models.
    expect(store.usage().map((r) => r.id)).toEqual(['u0', 'u1', 'u2', 'u3']);
    expect(store.usageTotal()).toBe(4);
    expect(store.usageTotalPages()).toBe(1);
    expect(store.usagePage()).toBe(1);
    expect(store.usageFacets().models).toEqual(['llama-3.2-3b']);
    expect(store.usageFacets().prefixes).toEqual(['7Qk4']);
    expect(fakeApi.lastUsageQuery?.page).toBe(1);
    expect(fakeApi.lastUsageQuery?.limit).toBe(25);
  });

  it('pages the request log against the matching total', async () => {
    fakeApi.usageRows = Array.from({ length: 60 }, (_, i) => makeUsageRowT(`u${i}`));
    await establishSession();
    // 60 rows at 25 per page: three pages, page one showing the newest batch.
    expect(store.usageTotal()).toBe(60);
    expect(store.usageTotalPages()).toBe(3);
    expect(store.usage().map((r) => r.id)).toEqual(
      Array.from({ length: 25 }, (_, i) => `u${i}`),
    );

    store.gotoPage(2);
    await tick();
    expect(fakeApi.lastUsageQuery?.page).toBe(2);
    expect(store.usage().map((r) => r.id)).toEqual(
      Array.from({ length: 25 }, (_, i) => `u${25 + i}`),
    );

    // Clamping: past the last page stays on it; a no-op when already there.
    store.gotoPage(99);
    await tick();
    expect(fakeApi.lastUsageQuery?.page).toBe(3);
    expect(store.usage().map((r) => r.id)).toEqual(
      Array.from({ length: 10 }, (_, i) => `u${50 + i}`),
    );
    const callsBefore = fakeApi.usageCalls;
    store.gotoPage(3);
    await tick();
    expect(fakeApi.usageCalls).toBe(callsBefore);
  });

  it('a structured filter resets to page one and re-fetches', async () => {
    fakeApi.usageRows = Array.from({ length: 60 }, (_, i) => makeUsageRowT(`u${i}`));
    await establishSession();
    store.gotoPage(3);
    await tick();
    expect(store.usagePage()).toBe(3);

    // Picking a key prefix is a new server query: back to page one, page 1
    // requested on the wire.
    store.setUsageKey('7Qk4');
    await tick();
    expect(store.usagePage()).toBe(1);
    expect(fakeApi.lastUsageQuery?.key).toBe('7Qk4');
    expect(fakeApi.lastUsageQuery?.page).toBe(1);

    // The model filter composes with the key filter and also resets.
    store.setUsageModel('llama-3.2-3b');
    await tick();
    expect(fakeApi.lastUsageQuery?.model).toBe('llama-3.2-3b');
    expect(fakeApi.lastUsageQuery?.key).toBe('7Qk4');

    // The date bounds ride the same query as unix seconds.
    const sec = secOf('2026-08-25T14:00:00Z');
    store.setUsageFrom(sec - 3600);
    store.setUsageTo(sec + 3600);
    await tick();
    expect(fakeApi.lastUsageQuery?.from).toBe(sec - 3600);
    expect(fakeApi.lastUsageQuery?.to).toBe(sec + 3600);
  });

  it('clearUsageFilters resets every structured filter to its default and re-fetches page one', async () => {
    fakeApi.usageRows = [makeUsageRowT('u0'), makeUsageRowT('u1')];
    await establishSession();
    store.setUsageKey('7Qk4');
    store.setUsageModel('llama-3.2-3b');
    store.setUsageFrom(1);
    store.setUsageTo(2);
    await tick();
    expect(store.usagePage()).toBe(1);

    store.clearUsageFilters();
    await tick();
    expect(store.usageKey()).toBe('');
    expect(store.usageModel()).toBe('');
    expect(store.usageFrom()).toBe(0);
    expect(store.usageTo()).toBe(0);
    expect(store.usageStatusFilter()).toBe('all');
    expect(store.usagePage()).toBe(1);
    expect(fakeApi.lastUsageQuery?.key).toBeUndefined();
    expect(fakeApi.lastUsageQuery?.model).toBeUndefined();
    expect(fakeApi.lastUsageQuery?.from).toBeUndefined();
    expect(fakeApi.lastUsageQuery?.to).toBeUndefined();
  });

  it('load more transactions appends older listing rows without duplicates', async () => {
    await establishSession();
    fakeApi.fundingListData = [makeFunding({ purchaseId: 'dep1' }), makeFunding({ purchaseId: 'dep2' })];
    fakeApi.listingsData = [makeListing('1', 'selling'), makeListing('0', 'buying')];
    fakeApi.listingsPageSize = 1;
    await store.loadListings();
    store.startPolling();
    await tick();
    // One market row in the first batch; deposits are always full. The
    // listings cursor is live, so the control would show.
    expect(store.listings().map((l) => l.id)).toEqual(['1']);
    expect(store.transactions().length).toBe(3);
    expect(store.transactionsHasMore()).toBe(true);

    await store.loadMoreTransactions();
    expect(store.listings().map((l) => l.id)).toEqual(['1', '0']);
    const ids = store.transactions().map((r) => r.id);
    expect(new Set(ids).size).toBe(ids.length);
    expect(store.transactionsHasMore()).toBe(false);

    // A further load-more is a no-op once history is exhausted.
    const callsBefore = fakeApi.listingsCalls;
    await store.loadMoreTransactions();
    expect(fakeApi.listingsCalls).toBe(callsBefore);
  });

  it('a same-context listings refresh merges over the preserved older pages', async () => {
    await establishSession();
    fakeApi.listingsData = [makeListing('1', 'selling'), makeListing('0', 'buying')];
    fakeApi.listingsPageSize = 1;
    await store.loadListings();
    await store.loadMoreTransactions();
    expect(store.listings().map((l) => l.id)).toEqual(['1', '0']);

    // The poll's own read re-fetches only the newest batch but must keep the
    // older page already loaded.
    await store.loadListings();
    expect(store.listings().map((l) => l.id)).toEqual(['1', '0']);
  });

  it('clears the session on a 401 instead of retrying forever', async () => {
    fakeApi.failNext = new ApiError(401, 'unauthorized', 'token expired');
    await establishSession();

    // The 401 arms the single recovery flow: session cleared, prompt up,
    // and no per-card expired-session error.
    expect(fakeWallet.token()).toBeNull();
    expect(fakeWallet.accountId()).toBeNull();
    expect(store.sessionExpired()).toBe(true);
    expect(store.balanceError()).toBeNull();
    expect(fakeApi.accountCalls).toBe(1);

    // The cleared session re-runs the effect (resetting the loadedFor guard);
    // flush it, then a fresh sign-in loads again. This is intent, not a retry loop.
    TestBed.flushEffects();
    await establishSession();
    expect(store.sessionExpired()).toBe(false);
    expect(fakeApi.accountCalls).toBe(2);
    expect(store.balance()).toBe(OPENING_BALANCE);
    expect(store.balanceError()).toBeNull();
  });

  it('collapses parallel 401s into a single recovery prompt', async () => {
    // Two loaders fail with 401 in the same flush; both must land in the
    // one recovery flow - no per-card errors, no prompt storm.
    fakeApi.failNext = new ApiError(401, 'unauthorized', 'token expired');
    fakeWallet.address.set(WALLET_ADDRESS);
    fakeWallet.accountId.set('0xmockacct');
    fakeWallet.token.set('mock-token');
    TestBed.flushEffects();
    await tick();
    // The initial load already fired; now make every read 401.
    const before = fakeApi.accountCalls;
    fakeApi.failNext = new ApiError(401, 'unauthorized', 'token expired');
    const p1 = store.loadAccount();
    const p2 = store.loadKeys();
    await Promise.all([p1, p2]);
    TestBed.flushEffects();

    expect(store.sessionExpired()).toBe(true);
    // No card shows its own expired-session text.
    expect(store.balanceError()).toBeNull();
    expect(store.keysError()).toBeNull();
    expect(fakeApi.accountCalls).toBe(before + 1);
  });

  it('reauth with an address and provider runs the SIWE flow once', async () => {
    await establishSession();
    fakeApi.failNext = new ApiError(401, 'unauthorized', 'token expired');
    await store.loadAccount();
    TestBed.flushEffects();
    expect(store.sessionExpired()).toBe(true);

    (fakeWallet as unknown as { provider: unknown }).provider = {};
    (fakeWallet as unknown as { startSessionCalls: number }).startSessionCalls = 0;
    store.reauth();
    expect((fakeWallet as unknown as { startSessionCalls: number }).startSessionCalls).toBe(1);
    // Still armed until the re-sign lands (or declines).
    expect(store.sessionExpired()).toBe(true);
  });

  it('abandons recovery when the re-sign is declined, landing clean', async () => {
    await establishSession();
    fakeApi.failNext = new ApiError(401, 'unauthorized', 'token expired');
    await store.loadAccount();
    TestBed.flushEffects();
    expect(store.sessionExpired()).toBe(true);

    // Simulate a decline: provider present, startSession publishes an error.
    (fakeWallet as unknown as { provider: unknown }).provider = {};
    (fakeWallet as unknown as { declineOnStartSession: boolean }).declineOnStartSession = true;
    store.reauth();
    TestBed.flushEffects();
    await tick();

    // Abandoned: prompt down, wallet cleared, no stuck cards, history gone.
    expect(store.sessionExpired()).toBe(false);
    expect(store.wallet()).toBeNull();
    expect(store.signedIn()).toBe(false);
    expect(store.balanceError()).toBeNull();
    expect(store.keysError()).toBeNull();
    expect(store.usage()).toEqual([]);
    expect(store.fundingHistory()).toEqual([]);
  });

  it('refreshes the list and the account after a mutation', async () => {
    await establishSession();
    const accountCallsBefore = fakeApi.accountCalls;

    await store.createKey('test');
    expect(store.keys().length).toBe(5);
    expect(store.keys()[0].label).toBe('test');
    expect(fakeApi.accountCalls).toBeGreaterThan(accountCallsBefore);

    await store.revokeKey('7Qk4');
    expect(store.keys().find((k) => k.id === '7Qk4')?.revoked).toBe(true);
    expect(store.activeKeys().some((k) => k.id === '7Qk4')).toBe(false);

    // A failed revoke leaves the row untouched and sets the error.
    fakeApi.failNext = new ApiError(500, 'internal', 'boom');
    await expect(store.revokeKey('2Mv9')).rejects.toSatisfy(
      (e: unknown) => e instanceof ApiError && e.status === 500,
    );
    expect(store.keys().find((k) => k.id === '2Mv9')?.revoked).toBe(false);
    expect(store.keysError()).not.toBeNull();
  });

  it('mints 1000 credits per dollar', async () => {
    await establishSession();
    store.setAmount(250);
    expect(store.creditsToBuy()).toBe(250_000);
  });

  it('refuses amounts outside the per-transaction limits', () => {
    store.setAmount(4);
    expect(store.amountValid()).toBe(false);
    store.setAmount(5);
    expect(store.amountValid()).toBe(true);
    store.setAmount(50_001);
    expect(store.amountValid()).toBe(false);
  });

  it('carries the chosen amount into the pay line and the credit figure', () => {
    store.setAmount(100);
    expect(store.payText()).toBe('100.00 USDC');
    expect(store.creditsToBuy()).toBe(100_000);
  });

  it('falls back to the mock address when no wallet is connected', () => {
    expect(store.address()).toBe(WALLET_ADDRESS);
  });

  it('a structured filter change re-fetches the paged read; the chart range does not', async () => {
    await establishSession();
    const callsAfterLoad = fakeApi.usageCalls;

    store.setUsageModel('llama-3.2-3b');
    await tick();
    expect(fakeApi.lastUsageQuery?.model).toBe('llama-3.2-3b');
    expect(fakeApi.lastUsageQuery?.page).toBe(1);
    expect(fakeApi.usageCalls).toBeGreaterThan(callsAfterLoad);

    // The chart range is not a log filter: changing it issues no new request.
    const callsBeforeDays = fakeApi.usageCalls;
    store.usageDays.set(7);
    await tick();
    expect(fakeApi.usageCalls).toBe(callsBeforeDays);
  });

  it('never charges a rejected request', async () => {
    await establishSession();
    const rejected = store.usage().find((r) => r.error !== null);
    expect(rejected?.credits).toBe(0);
  });

  it('status chips: Metered narrows the page client-side, Error and All re-fetch', async () => {
    await establishSession();
    const all = store.usage().length;
    const errors = store.usage().filter((r) => r.error !== null).length;

    // Metered has no server-side counterpart: it narrows the loaded page and
    // issues no request.
    const callsBeforeMetered = fakeApi.usageCalls;
    store.setUsageFilter('metered');
    expect(fakeApi.usageCalls).toBe(callsBeforeMetered);
    expect(store.filteredUsage().length).toBe(all - errors);
    expect(store.filteredUsage().every((r) => r.error === null)).toBe(true);

    // Error rides the server query (errors=true) and re-fetches from page one.
    store.setUsageFilter('error');
    await tick();
    expect(fakeApi.usageCalls).toBeGreaterThan(callsBeforeMetered);
    expect(fakeApi.lastUsageQuery?.errors).toBe(true);
    expect(store.usage().every((r) => r.error !== null)).toBe(true);

    // All drops the flag and re-fetches the unfiltered page.
    store.setUsageFilter('all');
    await tick();
    expect(fakeApi.lastUsageQuery?.errors).toBe(false);
    expect(store.usage().length).toBe(all);
  });

  it('the totals count the rows on the page after the status chip narrows them', async () => {
    await establishSession();
    const all = store.usage().length;
    const errors = store.usage().filter((r) => r.error !== null).length;
    expect(store.usageTotals().count).toBe(all);
    expect(store.usageCountText()).toBe(`${all} requests`);
    // Metered narrows client-side, so the totals drop to the settled rows
    // without a request.
    store.setUsageFilter('metered');
    expect(store.usageTotals().count).toBe(all - errors);
    expect(store.usageCountText()).toBe(`${all - errors} requests`);
  });

  // ---- inclusion proof (EP-027-T05) ------------------------------------

  it('maps the wire settlement facts onto the row commitment, all three together', async () => {
    await establishSession();
    const settled = store.usage().find((r) => r.id === 'u1');
    expect(settled).toBeDefined();
    expect(settled!.commitment).not.toBeNull();
    expect(settled!.commitment!.root.length).toBeGreaterThan(0);
    expect(settled!.commitment!.commitTx.length).toBeGreaterThan(0);
    expect(Number.isInteger(settled!.commitment!.batchIndex)).toBe(true);
    // The pending row and the error row both read as awaiting settlement.
    expect(store.usage().find((r) => r.id === 'u0')!.commitment).toBeNull();
    expect(store.usage().find((r) => r.id === 'u4')!.commitment).toBeNull();
  });

  it('verifyProof walks a good fixture proof and marks the row verified', async () => {
    await establishSession();
    const row = store.usage().find((r) => r.id === 'u1')!;
    await store.verifyProof(row);
    const check = store.proofChecks()['u1'];
    expect(check.state).toBe('verified');
    expect(fakeApi.usageProofCalls).toBe(1);
  });

  it('verifyProof marks the check failed when the served proof does not walk to the committed root', async () => {
    await establishSession();
    fakeApi.tamperNextProof = true;
    const row = store.usage().find((r) => r.id === 'u1')!;
    await store.verifyProof(row);
    const check = store.proofChecks()['u1'];
    expect(check.state).toBe('failed');
    expect(check.detail).toContain('does not match');
  });

  it('verifyProof marks the check error with the message when the proof read fails', async () => {
    await establishSession();
    fakeApi.usageProofFailNext = new ApiError(500, 'internal', 'nope');
    const row = store.usage().find((r) => r.id === 'u1')!;
    await store.verifyProof(row);
    const check = store.proofChecks()['u1'];
    expect(check.state).toBe('error');
    expect(check.detail).toBe('nope');
  });

  it('verifyProof is a no-op for a row without a commitment', async () => {
    await establishSession();
    const row = store.usage().find((r) => r.id === 'u0')!;
    expect(row.commitment).toBeNull();
    await store.verifyProof(row);
    expect(fakeApi.usageProofCalls).toBe(0);
    expect(store.proofChecks()['u0']).toBeUndefined();
  });

  it('resets the usage filters to their defaults on disconnect', async () => {
    await establishSession();
    store.setUsageFilter('metered');
    store.setUsageKey('7Qk4');
    store.setUsageModel('llama-3.2-3b');
    store.setUsageFrom(100);
    store.setUsageTo(200);
    expect(store.usageStatusFilter()).toBe('metered');
    expect(store.usageKey()).toBe('7Qk4');
    store.disconnect();
    expect(store.usageStatusFilter()).toBe('all');
    expect(store.usageKey()).toBe('');
    expect(store.usageModel()).toBe('');
    expect(store.usageFrom()).toBe(0);
    expect(store.usageTo()).toBe(0);
    expect(store.usagePage()).toBe(1);
    // Restore the session for the polling tests that follow.
    await establishSession();
  });

  it('disconnect resets every session-scoped error', async () => {
    await establishSession();

    // Populate every error card the store publishes, plus the wallet
    // services' flow state.
    store.balanceError.set('balance boom');
    store.chainsError.set('chains boom');
    store.modelsError.set('models boom');
    store.keysError.set('keys boom');
    store.usageError.set('usage boom');
    store.fundingError.set('funding boom');
    store.listingsError.set('listings boom');
    store.marketError.set('market boom');
    fakeSell.error.set('sell boom');
    fakeSell.step.set('cancelling');
    fakeSell.txHash.set('0xold');
    fakeBuy.error.set('buy boom');
    fakeBuy.step.set('buying');
    fakeBuy.txHash.set('0xold');
    fakeBuy.listingGone.set(true);
    fakeDeposit.error.set('deposit service boom');

    store.disconnect();

    expect(store.balanceError()).toBeNull();
    expect(store.chainsError()).toBeNull();
    expect(store.modelsError()).toBeNull();
    expect(store.keysError()).toBeNull();
    expect(store.usageError()).toBeNull();
    expect(store.fundingError()).toBeNull();
    expect(store.listingsError()).toBeNull();
    expect(store.marketError()).toBeNull();
    expect(fakeSell.error()).toBeNull();
    expect(fakeSell.step()).toBe('idle');
    expect(fakeSell.txHash()).toBeNull();
    expect(fakeBuy.error()).toBeNull();
    expect(fakeBuy.step()).toBe('idle');
    expect(fakeBuy.txHash()).toBeNull();
    expect(fakeBuy.listingGone()).toBe(false);
    expect(fakeDeposit.error()).toBeNull();
  });

  it('a new cancel clears the listings error card before the attempt', async () => {
    await establishSession();

    store.listingsError.set('stale rejection');
    fakeSell.hangNext = true;
    const p = store.cancelListing('7');
    p.catch(() => {});
    await Promise.resolve();
    // The card is cleared the moment the new action starts, even while the
    // prompt is still open.
    expect(store.listingsError()).toBeNull();
    expect(fakeSell.cancelCalls).toEqual(['7']);
  });

  it('reconnect with another account starts clean', async () => {
    await establishSession();

    store.balanceError.set('wallet 1 balance');
    store.listingsError.set('wallet 1 rejection');
    fakeSell.error.set('wallet 1 sell');

    store.disconnect();

    // A different wallet signs in: none of wallet 1's cards may survive.
    fakeWallet.address.set('0xotheraddress');
    fakeWallet.accountId.set('0xotheracct');
    fakeWallet.token.set('mock-token-2');
    TestBed.flushEffects();
    await tick();

    expect(store.balanceError()).toBeNull();
    expect(store.chainsError()).toBeNull();
    expect(store.modelsError()).toBeNull();
    expect(store.keysError()).toBeNull();
    expect(store.usageError()).toBeNull();
    expect(store.fundingError()).toBeNull();
    expect(store.listingsError()).toBeNull();
    expect(store.marketError()).toBeNull();
    expect(fakeSell.error()).toBeNull();
    expect(fakeBuy.error()).toBeNull();
    expect(fakeDeposit.error()).toBeNull();
    expect(fakeWallet.error()).toBeNull();
  });

  // ---- polling tests ---------------------------------------------------

  it('a tick refreshes every subscribed read, and keeps ticking', async () => {
    vi.useFakeTimers();
    try {
      await establishSessionFake();
      const account = fakeApi.accountCalls;
      const usage = fakeApi.usageCalls;
      const funding = fakeApi.fundingListCalls;
      const listings = fakeApi.listingsCalls;
      const market = fakeApi.marketCalls;
      const attestation = fakeApi.attestationCalls;

      // One interval later every live figure re-reads.
      await vi.advanceTimersByTimeAsync(15_000);
      expect(fakeApi.accountCalls).toBe(account + 1);
      expect(fakeApi.usageCalls).toBe(usage + 1);
      expect(fakeApi.fundingListCalls).toBe(funding + 1);
      expect(fakeApi.listingsCalls).toBe(listings + 1);
      expect(fakeApi.marketCalls).toBe(market + 1);
      expect(fakeApi.attestationCalls).toBe(attestation + 1);

      // And again: the loop is immortal, not a one-shot.
      await vi.advanceTimersByTimeAsync(15_000);
      expect(fakeApi.accountCalls).toBe(account + 2);
      expect(fakeApi.usageCalls).toBe(usage + 2);
      expect(fakeApi.fundingListCalls).toBe(funding + 2);
      expect(fakeApi.listingsCalls).toBe(listings + 2);
      expect(fakeApi.marketCalls).toBe(market + 2);
      expect(fakeApi.attestationCalls).toBe(attestation + 2);
    } finally {
      vi.useRealTimers();
    }
  });

  it('a failed tick surfaces the error but never stops later ticks', async () => {
    vi.useFakeTimers();
    try {
      await establishSessionFake();
      fakeApi.fundingListData = [makeFunding({ state: 'confirmed' })];
      const funding = fakeApi.fundingListCalls;
      const market = fakeApi.marketCalls;
      expect(store.balance()).toBe(OPENING_BALANCE);

      // Every read fails: each surfaces its own error, stale values stay up,
      // and the loop keeps firing.
      fakeApi.alwaysFail = new ApiError(503, 'unavailable', 'nope');
      await vi.advanceTimersByTimeAsync(15_000);
      expect(store.balanceError()).not.toBeNull();
      expect(store.usageError()).not.toBeNull();
      expect(store.fundingError()).not.toBeNull();
      expect(store.listingsError()).not.toBeNull();
      expect(store.marketError()).not.toBeNull();
      // Stale figures survive the failure.
      expect(store.balance()).toBe(OPENING_BALANCE);
      // The loop did not die: it still polls on the next tick.
      expect(fakeApi.fundingListCalls).toBe(funding + 1);
      await vi.advanceTimersByTimeAsync(15_000);
      expect(fakeApi.fundingListCalls).toBe(funding + 2);
      expect(fakeApi.marketCalls).toBe(market + 2);

      // Recovery: clear the failure; the next tick updates values and clears
      // the errors in place.
      fakeApi.alwaysFail = null;
      await vi.advanceTimersByTimeAsync(15_000);
      expect(store.balanceError()).toBeNull();
      expect(store.usageError()).toBeNull();
      expect(store.fundingError()).toBeNull();
      expect(store.listingsError()).toBeNull();
      expect(store.marketError()).toBeNull();
    } finally {
      vi.useRealTimers();
    }
  });

  it('pauses while hidden and resumes with an immediate refresh when visible', async () => {
    vi.useFakeTimers();
    try {
      await establishSessionFake();
      const funding = fakeApi.fundingListCalls;
      const account = fakeApi.accountCalls;

      // Hide the tab: no interval, so no requests however long it stays down.
      Object.defineProperty(document, 'hidden', { value: true, configurable: true });
      document.dispatchEvent(new Event('visibilitychange'));
      await vi.advanceTimersByTimeAsync(30_000);
      expect(fakeApi.fundingListCalls).toBe(funding);
      expect(fakeApi.accountCalls).toBe(account);

      // Show it again: an immediate catch-up refresh fires, then the interval
      // resumes for another tick at 15 s.
      Object.defineProperty(document, 'hidden', { value: false, configurable: true });
      document.dispatchEvent(new Event('visibilitychange'));
      await vi.advanceTimersByTimeAsync(0);
      expect(fakeApi.fundingListCalls).toBe(funding + 1);
      expect(fakeApi.accountCalls).toBe(account + 1);
      await vi.advanceTimersByTimeAsync(15_000);
      expect(fakeApi.fundingListCalls).toBe(funding + 2);
    } finally {
      vi.useRealTimers();
    }
  });

  it('polls nothing while signed out', async () => {
    vi.useFakeTimers();
    try {
      // No session yet: nothing polls.
      await vi.advanceTimersByTimeAsync(30_000);
      expect(fakeApi.accountCalls).toBe(0);
      expect(fakeApi.fundingListCalls).toBe(0);
      expect(fakeApi.listingsCalls).toBe(0);
      expect(fakeApi.marketCalls).toBe(0);
      expect(fakeApi.usageCalls).toBe(0);

      // A session starts the loop; disconnecting stops it again.
      await establishSessionFake();
      const account = fakeApi.accountCalls;
      const funding = fakeApi.fundingListCalls;
      store.disconnect();
      TestBed.flushEffects();
      await vi.advanceTimersByTimeAsync(30_000);
      expect(fakeApi.accountCalls).toBe(account);
      expect(fakeApi.fundingListCalls).toBe(funding);
    } finally {
      vi.useRealTimers();
    }
  });

  it('pagination state survives a tick', async () => {
    vi.useFakeTimers();
    try {
      fakeApi.usageRows = Array.from({ length: 60 }, (_, i) => makeUsageRowT(`u${i}`));
      fakeApi.listingsData = [
        makeListing('3', 'selling'),
        makeListing('2', 'selling'),
        makeListing('1', 'selling'),
      ];
      fakeApi.listingsPageSize = 1;
      await establishSessionFake();
      await store.loadListings();
      await store.loadMoreTransactions();
      expect(store.listings().map((l) => l.id)).toEqual(['3', '2']);
      expect(store.transactionsHasMore()).toBe(true);

      // A tick re-reads only the newest batch but must keep the older page
      // already loaded and never walk the cursor back up.
      await vi.advanceTimersByTimeAsync(15_000);
      expect(store.listings().map((l) => l.id)).toEqual(['3', '2']);
      expect(store.transactionsHasMore()).toBe(true);

      // The request log keeps its page and filters across a tick too.
      store.gotoPage(2);
      await vi.advanceTimersByTimeAsync(0);
      expect(store.usagePage()).toBe(2);
      expect(fakeApi.lastUsageQuery?.page).toBe(2);
      expect(store.usageTotal()).toBe(60);
      await vi.advanceTimersByTimeAsync(15_000);
      expect(store.usagePage()).toBe(2);
      expect(fakeApi.lastUsageQuery?.page).toBe(2);
      expect(store.usageTotal()).toBe(60);
    } finally {
      vi.useRealTimers();
    }
  });

  it('a 401 on a tick hands off to recovery and resumes after re-auth', async () => {
    vi.useFakeTimers();
    try {
      await establishSessionFake();
      const funding = fakeApi.fundingListCalls;
      const account = fakeApi.accountCalls;

      // A 401 on any tick read drops the session and stops the loop.
      fakeApi.alwaysFail = new ApiError(401, 'unauthorized', 'token expired');
      await vi.advanceTimersByTimeAsync(15_000);
      expect(store.sessionExpired()).toBe(true);
      expect(fakeWallet.token()).toBeNull();
      // Nothing further polls while signed out.
      await vi.advanceTimersByTimeAsync(30_000);
      expect(fakeApi.fundingListCalls).toBe(funding + 1);
      expect(fakeApi.accountCalls).toBe(account + 1);

      // Re-sign: recovery clears, the session effect re-runs, polling resumes.
      fakeApi.alwaysFail = null;
      await establishSessionFake();
      await vi.advanceTimersByTimeAsync(15_000);
      expect(fakeApi.fundingListCalls).toBeGreaterThan(funding + 1);
    } finally {
      vi.useRealTimers();
    }
  });

  it('a settled trace keeps a stable figure across ticks', async () => {
    vi.useFakeTimers();
    try {
      const settled = makeFunding({
        state: 'settled', ethBlock: 100, attestedHeight: 100,
        builderCeiling: 100, ccTx: '0xabc', ccBlock: 50, credits: 250_000,
        startedAt: '2026-08-24T00:14:24Z', updatedAt: '2026-08-24T00:24:24Z',
      });
      fakeApi.fundingListData = [settled];
      await establishSessionFake();
      expect(store.traceComplete()).toBe(true);
      const elapsed = store.elapsedText();
      // A later tick re-reads the funding list but the settled figure is
      // frozen at start-to-credit, so it does not move.
      await vi.advanceTimersByTimeAsync(15_000);
      expect(store.fundingStatus()?.state).toBe('settled');
      expect(store.elapsedText()).toBe(elapsed);
    } finally {
      vi.useRealTimers();
    }
  });

  it('builder below attested does not hold the trace back in the store', async () => {
    const lagging = makeFunding({
      ethBlock: 100,
      attestedHeight: 200,
      chainTip: 250,
      builderCeiling: 150,
    });
    fakeApi.fundingListData = [lagging];

    store.startPolling();
    await tick();

    expect(store.fundingStatus()?.builderCeiling).toBe(150);
    const steps = store.steps();
    expect(steps.length).toBe(3);
    // Deposit seen and height attested; the mint is live and nothing after it is pending.
    expect(steps[0].state).toBe('done');
    expect(steps[1].state).toBe('done');
    expect(steps[2].state).toBe('live');
  });

  it('on load, shows an in-flight purchase from the endpoint', async () => {
    const inFlight = makeFunding({
      state: 'confirmed',
      ethBlock: 100,
      attestedHeight: 100,
      chainTip: 150,
    });
    fakeApi.fundingListData = [inFlight];

    store.startPolling();
    await tick();

    expect(store.fundingStatus()?.purchaseId).toBe('p1');
    expect(store.traceComplete()).toBe(false);
    expect(store.stepLabel()).toBe('Step 3 of 3');
  });

  it('selecting a purchase from the history shows its trace and survives the next poll', async () => {
    const older = makeFunding({ purchaseId: 'p0', state: 'settled', ethBlock: 90, attestedHeight: 90, builderCeiling: 90, ccTx: '0xold', ccBlock: 40, credits: 5000 });
    const inFlight = makeFunding({ purchaseId: 'p1', state: 'confirmed', ethBlock: 100, attestedHeight: 100, chainTip: 150 });
    fakeApi.fundingListData = [inFlight, older];

    store.startPolling();
    await tick();
    expect(store.fundingHistory().map((p) => p.purchaseId)).toEqual(['p1', 'p0']);
    expect(store.fundingStatus()?.purchaseId).toBe('p1');

    store.selectPurchase('p0');
    expect(store.fundingStatus()?.purchaseId).toBe('p0');
    expect(store.traceComplete()).toBe(true);

    store.startPolling();
    await tick();
    expect(store.fundingStatus()?.purchaseId).toBe('p0');

    store.selectPurchase('nope');
    expect(store.fundingStatus()?.purchaseId).toBe('p0');
  });

  it('elapsed time freezes at start-to-credit once a purchase is settled', async () => {
    const settled = makeFunding({
      state: 'settled', ethBlock: 100, attestedHeight: 100, builderCeiling: 100, ccTx: '0xabc', ccBlock: 50, credits: 5000,
      startedAt: '2026-08-24T00:14:24Z', updatedAt: '2026-08-24T00:24:24Z',
    });
    fakeApi.fundingListData = [settled];
    store.startPolling();
    await tick();
    expect(store.elapsedText()).toBe('10:00');
  });

  it('a direct Solana purchase (chain 0) shows one step and a frozen elapsed figure', async () => {
    const direct = makeFunding({
      purchaseId: 'd1', state: 'settled', chainKey: 0, chainName: 'Solana',
      ethBlock: 404, ethTx: '0xdirect',
      ccTx: '0xdirect', ccBlock: 404, credits: 250_000,
      startedAt: '2026-08-24T00:00:00Z', updatedAt: '2026-08-24T00:00:13Z',
    });
    fakeApi.fundingListData = [direct];
    store.startPolling();
    await tick();
    expect(store.fundingStatus()?.purchaseId).toBe('d1');
    expect(store.steps().length).toBe(1);
    expect(store.steps()[0].lane).toBe('cc');
    expect(store.traceComplete()).toBe(true);
    // Payment to credit is one block: a settled figure, not a running clock.
    expect(store.elapsedText()).toBe('00:13');
    expect(store.stepLabel()).toBe('Complete');

    // A later poll must not move the figure.
    store.startPolling();
    await tick();
    expect(store.elapsedText()).toBe('00:13');
  });

  it('a payment just sent shows as pending until the backend reports it', async () => {
    fakeApi.fundingListData = [];
    store.announcePurchase('0xnew', 25, 'mUSDC');
    await tick();
    expect(store.fundingHistory().map((p) => p.purchaseId)).toEqual(['0xnew']);
    expect(store.fundingStatus()?.state).toBe('pending');
    // A just-sent deposit is one step, not a three-step proof journey.
    expect(store.stepLabel()).toBe('Step 1 of 1');

    // Still unknown to the backend on the next poll: stays at the top.
    store.startPolling();
    await tick();
    expect(store.fundingHistory().length).toBe(1);

    // The backend lists it under the same id: the server row replaces it.
    fakeApi.fundingListData = [makeFunding({ purchaseId: '0xnew', ethTx: '0xnew', state: 'confirmed', chainKey: 0, chainName: 'Solana', ethBlock: 100 })];
    store.startPolling();
    await tick();
    expect(store.fundingHistory().length).toBe(1);
    expect(store.fundingStatus()?.state).toBe('confirmed');
  });

  it('a direct payment announces a key-0 row the trace renders as one step', async () => {
    fakeApi.fundingListData = [];
    store.announcePurchase('0xdirect', 25, 'mUSD1', 0, 'Solana', true);
    await tick();
    expect(store.fundingStatus()?.chainKey).toBe(0);
    expect(store.fundingStatus()?.ethBlock).toBe(0);
    expect(store.steps().length).toBe(1);
    expect(store.steps()[0].lane).toBe('cc');
  });

  it('derives pre-mint credits from the deposit amount and hands over seamlessly when the mint lands', async () => {
    // The deposit fixes both figures the moment it is seen: 1,000 credits
    // per dollar of the deposit amount.
    fakeApi.fundingListData = [makeFunding({ credits: null, amountUsd: 250 })];
    store.startPolling();
    await tick();
    expect(store.purchase().credits).toBe(250_000);
    // The server's minted credits take over - the same number.
    fakeApi.fundingListData = [makeFunding({ credits: 250_000, amountUsd: 250 })];
    store.startPolling();
    await tick();
    expect(store.purchase().credits).toBe(250_000);
  });

  it('elapsed time of an in-flight purchase advances every second while polling', async () => {
    vi.useFakeTimers();
    try {
      const startedAt = new Date(Date.now() - 60_000).toISOString();
      fakeApi.fundingListData = [makeFunding({ state: 'confirmed', ethBlock: 100, startedAt })];
      store.startPolling();
      await vi.advanceTimersByTimeAsync(0);
      const before = store.elapsedText();
      await vi.advanceTimersByTimeAsync(3_000);
      expect(store.elapsedText()).not.toBe(before);
      expect(store.elapsedText()).toBe('01:03');
      store.stopPolling();
    } finally {
      vi.useRealTimers();
    }
  });

  // ---- market (buy) tests --------------------------------------------

  it('loadMarket passes the wire mine/matured straight through; no address derivation', async () => {
    await establishSession();
    fakeApi.marketData = [
      { id: '12', credits: 5_000, price: 500, token: 'mUSD1', maturesAt: 1756000000, matured: true, mine: false },
      { id: '3', credits: 2_000, price: 200, token: 'mUSD1', maturesAt: 1755900000, matured: true, mine: true },
    ];
    await store.loadMarket();
    expect(store.market().length).toBe(2);
    expect(store.market()[0].mine).toBe(false);
    expect(store.market()[1].mine).toBe(true);
    expect(store.buyableCount()).toBe(1);
    expect(store.forSaleCount()).toBe(2);
    expect(store.marketError()).toBeNull();
  });

  it('forSaleCount counts every open listing; buyableCount excludes own and unmatured', async () => {
    await establishSession();
    fakeApi.marketData = [
      { id: '3', credits: 2_000, price: 200, token: 'mUSD1', maturesAt: 1755900000, matured: false, mine: true },
      { id: '12', credits: 5_000, price: 500, token: 'mUSD1', maturesAt: 1756000000, matured: true, mine: false },
    ];
    await store.loadMarket();
    expect(store.forSaleCount()).toBe(2);
    expect(store.buyableCount()).toBe(1);
  });

  it('announceBuy publishes a complete card and reloads account, listings and market', async () => {
    await establishSession();
    const accountCallsBefore = fakeApi.accountCalls;
    const listingsCallsBefore = fakeApi.listingsCalls;
    const marketCallsBefore = fakeApi.marketCalls;

    store.announceBuy('0xbuytx', 5_000, '5.00 USDC');
    await tick();

    const card = store.buyCard();
    expect(card).not.toBeNull();
    // The settlement call returns only after the credits moved: complete
    // from the first frame, with no confirmation loop behind it.
    expect(card!.complete).toBe(true);
    expect(card!.credits).toBe(5_000);
    expect(card!.priceText).toBe('5.00 USDC');
    expect(card!.href).toContain('explorer.solana.com/tx/0xbuytx');
    // No local twin row: the card is the only announcement surface.
    expect(store.listings().some((l) => l.id === '0xbuytx')).toBe(false);

    // The three reloads ran so the balance, the listings and the market
    // all reflect the settled trade.
    expect(fakeApi.accountCalls).toBeGreaterThan(accountCallsBefore);
    expect(fakeApi.listingsCalls).toBeGreaterThan(listingsCallsBefore);
    expect(fakeApi.marketCalls).toBeGreaterThan(marketCallsBefore);
  });

  // ---- locked surfaces ignore buying rows ----------------------------

  it('locked surfaces ignore buying rows', async () => {
    await establishSession();
    fakeApi.listingsData = [
      { id: '1', listedTx: '0xlist1', state: 'open', role: 'selling', credits: 5_000, price: 500, token: 'mUSD1', matured: true, createdAt: '2026-08-22T20:00:00Z' },
      { id: 'b1', listedTx: '0xbuytx1', state: 'open', role: 'buying', credits: 5_000, price: 500, token: 'mUSD1', matured: true, createdAt: '2026-08-23T00:00:00Z' },
    ];
    await store.loadListings();

    // Only the sell-side open row counts toward the open-sell figure.
    expect(store.openListingsCount()).toBe(1);
    expect(store.sellListings().map((l) => l.id)).toEqual(['1']);
    // The locked total is the wire figure, untouched by the buying row.
    expect(store.locked()).toBe(OPENING_LOCKED);

    // A pending buying twin does not count as an open sell order either.
    store.announceBuy('0xbuytx', 5_000, '5.00 USDC');
    await Promise.resolve();
    expect(store.openListingsCount()).toBe(1);
    expect(store.sellListings().map((l) => l.id)).toEqual(['1']);
    expect(store.locked()).toBe(OPENING_LOCKED);
  });

  // ---- post-sell lock confirmation tests ------------------------------

  it('after a listing confirmation the store polls until the account locked figure changes, then stops', async () => {
    await establishSession();
    expect(store.locked()).toBe(OPENING_LOCKED);

    vi.useFakeTimers();
    try {
      const accountCallsBefore = fakeApi.accountCalls;
      store.announceListing(makeNewListingRow());
      await vi.advanceTimersByTimeAsync(0);
      // The immediate reload ran; the figure is unchanged so the loop is alive.
      expect(store.locked()).toBe(OPENING_LOCKED);
      expect(fakeApi.accountCalls).toBeGreaterThan(accountCallsBefore);

      // Bounded retry while the mirror lags: the figure has not moved yet.
      await vi.advanceTimersByTimeAsync(2_000);
      expect(fakeApi.accountCalls).toBeGreaterThan(accountCallsBefore + 1);

      // The feeder mirrors the lock; the next tick's reload picks it up and
      // the tick after confirms the move and clears the timer.
      fakeApi.accountLocked = OPENING_LOCKED + 5_000;
      await vi.advanceTimersByTimeAsync(2_000);
      expect(store.locked()).toBe(OPENING_LOCKED + 5_000);

      await vi.advanceTimersByTimeAsync(2_000);
      const callsAfterMove = fakeApi.accountCalls;
      await vi.advanceTimersByTimeAsync(4_000);
      expect(fakeApi.accountCalls).toBe(callsAfterMove);
    } finally {
      vi.useRealTimers();
    }
  });

  it('a listing confirmation moves spendable and locked together in one poll cycle', async () => {
    await establishSession();
    expect(store.balance()).toBe(OPENING_BALANCE);
    expect(store.locked()).toBe(OPENING_LOCKED);

    vi.useFakeTimers();
    try {
      const accountCallsBefore = fakeApi.accountCalls;
      store.announceListing(makeNewListingRow());
      await vi.advanceTimersByTimeAsync(0);
      // The immediate full read ran; the mirror has not caught up yet, so
      // both figures are still the opening values.
      expect(store.balance()).toBe(OPENING_BALANCE);
      expect(store.locked()).toBe(OPENING_LOCKED);
      expect(fakeApi.accountCalls).toBeGreaterThan(accountCallsBefore);

      // The server mirrors the trade in one response: 5_000 credits leave
      // spendable and join the locked figure in the same account read.
      fakeApi.accountBalance = OPENING_BALANCE - 5_000;
      fakeApi.accountLocked = OPENING_LOCKED + 5_000;
      await vi.advanceTimersByTimeAsync(2_000);
      // Both surfaces update together in the same tick: no refresh can
      // half-update the screen.
      expect(store.balance()).toBe(OPENING_BALANCE - 5_000);
      expect(store.locked()).toBe(OPENING_LOCKED + 5_000);

      // One more tick confirms the move and clears the timer.
      const callsAfterMove = fakeApi.accountCalls;
      await vi.advanceTimersByTimeAsync(4_000);
      expect(fakeApi.accountCalls).toBe(callsAfterMove);
    } finally {
      vi.useRealTimers();
    }
  });

  it('a cancel confirmation polls until the lock drops, and a failed cancel does not start the loop', async () => {
    const openRow: Listing = {
      id: '7', listedTx: '0xlist7', state: 'open', role: 'selling', credits: 5_000, price: 500,
      token: 'mUSD1', matured: false, createdAt: '2026-08-22T20:00:00Z',
    };
    fakeApi.listingsData = [openRow];
    await establishSession();
    await store.loadListings();
    expect(store.locked()).toBe(OPENING_LOCKED);

    vi.useFakeTimers();
    try {
      const accountCallsBefore = fakeApi.accountCalls;
      await store.cancelListing('7');
      await vi.advanceTimersByTimeAsync(0);
      expect(fakeSell.cancelCalls).toEqual(['7']);
      // The immediate reload ran; the lock has not dropped yet.
      expect(store.locked()).toBe(OPENING_LOCKED);
      expect(fakeApi.accountCalls).toBeGreaterThan(accountCallsBefore);

      // The feeder mirrors the release in one response: the lock drops and
      // the freed credits return to spendable in the same account read.
      fakeApi.accountLocked = 0;
      fakeApi.accountBalance = OPENING_BALANCE + 5_000;
      await vi.advanceTimersByTimeAsync(2_000);
      expect(store.locked()).toBe(0);
      expect(store.balance()).toBe(OPENING_BALANCE + 5_000);

      await vi.advanceTimersByTimeAsync(2_000);
      const callsAfterMove = fakeApi.accountCalls;
      await vi.advanceTimersByTimeAsync(4_000);
      expect(fakeApi.accountCalls).toBe(callsAfterMove);
    } finally {
      vi.useRealTimers();
    }

    // A failed cancel leaves the row open and starts no confirmation loop.
    fakeSell.failNext = new Error('wallet rejected');
    const callsBeforeFail = fakeApi.accountCalls;
    await expect(store.cancelListing('7')).rejects.toThrow();
    await tick();
    expect(fakeApi.accountCalls).toBe(callsBeforeFail);
  });

  it('the post-sell lock confirmation is bounded by the ceiling when the figure never moves', async () => {
    await establishSession();
    vi.useFakeTimers();
    try {
      const accountCallsBefore = fakeApi.accountCalls;
      store.announceListing(makeNewListingRow());
      await vi.advanceTimersByTimeAsync(0);
      // Immediate reload plus LISTING_CONFIRM_MAX - 1 retry loads: the tick
      // that reaches the ceiling stops without reloading.
      for (let i = 0; i < 3; i += 1) {
        await vi.advanceTimersByTimeAsync(2_000);
      }
      expect(fakeApi.accountCalls).toBe(accountCallsBefore + 3);
      // The timer is cleared: no further ticks fire.
      await vi.advanceTimersByTimeAsync(4_000);
      expect(fakeApi.accountCalls).toBe(accountCallsBefore + 3);
    } finally {
      vi.useRealTimers();
    }
  });

  it('while an open listing exists the ordinary poll re-reads the account each tick', async () => {
    fakeApi.listingsData = [
      { id: '1', listedTx: '0xlist1', state: 'open', role: 'selling', credits: 5_000, price: 500, token: 'mUSD1', matured: true, createdAt: '2026-08-22T20:00:00Z' },
    ];
    vi.useFakeTimers();
    try {
      fakeWallet.address.set(WALLET_ADDRESS);
      fakeWallet.accountId.set('0xmockacct');
      fakeWallet.token.set('mock-token');
      TestBed.flushEffects();
      await vi.advanceTimersByTimeAsync(0);
      expect(store.openListingsCount()).toBe(1);

      // The ordinary 15 s cycle carries an account read while a lock can move.
      const accountCallsBefore = fakeApi.accountCalls;
      await vi.advanceTimersByTimeAsync(15_000);
      expect(fakeApi.accountCalls).toBeGreaterThan(accountCallsBefore);

      // A buyer takes the listing between ticks; the next account read
      // surfaces the new lock without navigation or user action.
      fakeApi.accountLocked = OPENING_LOCKED - 5_000;
      await vi.advanceTimersByTimeAsync(15_000);
      expect(store.locked()).toBe(OPENING_LOCKED - 5_000);
    } finally {
      vi.useRealTimers();
    }
  });

  it('the ordinary poll keeps ticking once all listings close and funding is idle', async () => {
    fakeApi.listingsData = [
      { id: '1', listedTx: '0xlist1', state: 'open', role: 'selling', credits: 5_000, price: 500, token: 'mUSD1', matured: true, createdAt: '2026-08-22T20:00:00Z' },
    ];
    vi.useFakeTimers();
    try {
      fakeWallet.address.set(WALLET_ADDRESS);
      fakeWallet.accountId.set('0xmockacct');
      fakeWallet.token.set('mock-token');
      TestBed.flushEffects();
      await vi.advanceTimersByTimeAsync(0);
      expect(store.openListingsCount()).toBe(1);

      // The buyer's sale is mirrored: the row closes on the next listings read.
      fakeApi.listingsData = [
        { id: '1', listedTx: '0xlist1', state: 'sold', role: 'selling', credits: 5_000, price: 500, token: 'mUSD1', matured: true, createdAt: '2026-08-22T20:00:00Z' },
      ];
      await vi.advanceTimersByTimeAsync(15_000);
      expect(store.openListingsCount()).toBe(0);

      // Nothing open, nothing in flight: the loop is immortal, so it keeps
      // refreshing the live figures on the next tick.
      const accountCallsAfterClose = fakeApi.accountCalls;
      const fundingCallsAfterClose = fakeApi.fundingListCalls;
      await vi.advanceTimersByTimeAsync(15_000);
      expect(fakeApi.accountCalls).toBe(accountCallsAfterClose + 1);
      expect(fakeApi.fundingListCalls).toBe(fundingCallsAfterClose + 1);
    } finally {
      vi.useRealTimers();
    }
  });

  // ---- metering stream tests ------------------------------------------

  it('a usage event prepends a row and updates the totals row', async () => {
    await establishSession();
    const es = FakeEventSource.instances.at(-1);
    expect(es).toBeDefined();

    const countBefore = store.usage().length;
    const totalsBefore = store.usageTotals();

    es!.emit('usage', {
      id: 'u-new',
      occurredAt: '2026-08-23T00:00:00Z',
      keyId: '7Qk4',
      model: 'gpt-x',
      promptTokens: 1_000_000,
      cachedTokens: 0,
      completionTokens: 100_000,
      credits: 13_000,
      status: 'ok',
      latencyMs: 120,
      error: null,
    });

    const rows = store.usage();
    expect(rows.length).toBe(countBefore + 1);
    expect(rows[0].key).toBe('7Qk4');
    expect(rows[0].model).toBe('gpt-x');
    expect(rows[0].credits).toBeCloseTo(1.3, 4);
    // The totals row is derived from the same signal.
    expect(store.usageTotals().count).toBe(totalsBefore.count + 1);
    expect(store.usageTotals().credits).toBeCloseTo(
      totalsBefore.credits + 1.3,
      4,
    );
  });

  it('a balance event updates every place the balance is rendered', async () => {
    await establishSession();
    expect(store.balance()).toBe(OPENING_BALANCE);
    const es = FakeEventSource.instances.at(-1);
    expect(es).toBeDefined();

    es!.emit('balance', { spendableMicro: 410_000 * 10_000 });

    expect(store.balance()).toBe(410_000);
    expect(store.balanceError()).toBeNull();
    // Every rendered balance view is a computed over the one signal.
    expect(store.balanceText()).toBe(creditText1(410_000));
    expect(store.balanceUsd()).toBe(usd(410_000));
  });

  it('a balance event carrying spendable and locked updates both figures', async () => {
    await establishSession();
    const es = FakeEventSource.instances.at(-1);
    expect(es).toBeDefined();

    es!.emit('balance', { spendableMicro: 300_000 * 10_000, lockedMicro: 50_000 * 10_000 });

    // One payload, two distinct figures: the headline and the lock.
    expect(store.balance()).toBe(300_000);
    expect(store.locked()).toBe(50_000);
    expect(store.balanceText()).toBe(creditText1(300_000));
    expect(store.lockedText()).toBe(creditText1(50_000));
    expect(store.balanceUsd()).not.toBe(store.lockedUsd());
  });

  it('a fractional balance event keeps its sub-credit part visible', async () => {
    await establishSession();
    const es = FakeEventSource.instances.at(-1);
    expect(es).toBeDefined();

    es!.emit('balance', { spendableMicro: 300_000_005 });

    expect(store.balance()).toBeCloseTo(30_000.0005, 5);
    // The balance display floors to one decimal: never overstated, and
    // whole-number balances keep their trailing ".0".
    expect(store.balanceText()).toBe('30,000.0');
  });

  // ---- listings tests --------------------------------------------------

  it('a listing just sent shows as open immediately and is replaced by the server row', async () => {
    fakeApi.listingsData = [];
    store.announceListing(makeNewListingRow());
    await tick();

    // The D57 local row carries the tx hash as its id.
    expect(store.listings().map((l) => l.id)).toEqual(['0xnewtx']);
    expect(store.listings()[0].state).toBe('open');
    expect(store.openListingsCount()).toBe(1);

    // Still unknown to the backend on the next read: stays at the top.
    await store.loadListings();
    expect(store.listings().length).toBe(1);

    // The backend lists it: same listedTx, its own numeric listing id.
    // The server row replaces the local twin; the row's id becomes numeric
    // so cancel() can take it.
    fakeApi.listingsData = [
      { id: '7', listedTx: '0xnewtx', state: 'open', role: 'selling', credits: 5_000, price: 500, token: 'mUSD1', matured: false, createdAt: '2026-08-22T20:00:00Z' },
    ];
    await store.loadListings();
    expect(store.listings().length).toBe(1);
    expect(store.listings()[0].id).toBe('7');
    expect(store.listings()[0].createdAt).toBe('2026-08-22T20:00:00Z');
    expect(store.openListingsCount()).toBe(1);
  });

  it('announceBuy carries the figure to the card without inventing a listing row', async () => {
    fakeApi.listingsData = [];
    store.announceBuy('0xbuytx', 5_000, '5.00 USDC');
    await tick();

    const card = store.buyCard();
    expect(card!.priceText).toBe('5.00 USDC');
    expect(card!.href).toContain('/tx/0xbuytx');
    // The card is the only surface: no local twin to be replaced later.
    expect(store.listings()).toEqual([]);
  });

  it('an announced listing keeps the raw price, pay token address and decimals it arrived with', async () => {
    fakeApi.listingsData = [];
    store.announceListing(makeNewListingRow());
    await tick();

    const local = store.listings()[0];
    // The row arrives fully shaped from the create call; the store stores
    // it as is, so it renders like every server row instead of a wrong or
    // raw-marked figure.
    expect(local.price).toBe(500_000_000);
    expect(local.tokenAddress).toBe('DemoUSDCmint11111111111111111111111111111111');
    expect(local.tokenDecimals).toBe(6);
  });

  it('cancel leaves the row until the server refresh changes it', async () => {
    const openRow: Listing = {
      id: '7', listedTx: '0xlist7', state: 'open', role: 'selling', credits: 5_000, price: 500,
      token: 'mUSD1', matured: false, createdAt: '2026-08-22T20:00:00Z',
    };
    fakeApi.listingsData = [openRow];
    await store.loadListings();
    expect(store.listings()[0].state).toBe('open');

    // The cancel transaction lands; the server still reports open.
    fakeApi.listingsData = [openRow];
    await store.cancelListing('7');
    expect(fakeSell.cancelCalls).toEqual(['7']);
    expect(store.listings()[0].state).toBe('open');

    // The server moves the row to cancelled; the next cancel refresh follows it.
    fakeApi.listingsData = [{ ...openRow, state: 'cancelled' }];
    await store.cancelListing('7').catch(() => {});
    expect(store.listings()[0].state).toBe('cancelled');
    expect(store.openListingsCount()).toBe(0);
  });

  it('a failed cancel leaves the row open and sets the listings error', async () => {
    const openRow: Listing = {
      id: '7', listedTx: '0xlist7', state: 'open', role: 'selling', credits: 5_000, price: 500,
      token: 'mUSD1', matured: false, createdAt: '2026-08-22T20:00:00Z',
    };
    fakeApi.listingsData = [openRow];
    await store.loadListings();

    fakeSell.failNext = new Error('wallet rejected');
    await expect(store.cancelListing('7')).rejects.toThrow();
    expect(fakeSell.cancelCalls).toEqual(['7']);
    expect(store.listings()[0].state).toBe('open');
    expect(store.listingsError()).not.toBeNull();
  });

  it('loads the account listings with a session and keeps polling when nothing is open', async () => {
    fakeApi.listingsData = [
      { id: '1', listedTx: '0xlist1', state: 'open', role: 'selling', credits: 5_000, price: 500, token: 'mUSD1', matured: true, createdAt: '2026-08-22T20:00:00Z' },
    ];
    vi.useFakeTimers();
    try {
      fakeWallet.address.set(WALLET_ADDRESS);
      fakeWallet.accountId.set('0xmockacct');
      fakeWallet.token.set('mock-token');
      TestBed.flushEffects();
      await vi.advanceTimersByTimeAsync(0);
      const callsAfterSession = fakeApi.listingsCalls;
      expect(callsAfterSession).toBeGreaterThan(0);
      expect(store.listings()[0].id).toBe('1');
      expect(store.openListingsCount()).toBe(1);

      // A listing that was already open on chain is re-polled: the interval
      // fires and refetches while the row stays open.
      await vi.advanceTimersByTimeAsync(15_000);
      expect(fakeApi.listingsCalls).toBe(callsAfterSession + 1);

      // All rows close: the loop is immortal, so it keeps ticking.
      fakeApi.listingsData = [
        { id: '1', listedTx: '0xlist1', state: 'sold', role: 'selling', credits: 5_000, price: 500, token: 'mUSD1', matured: true, createdAt: '2026-08-22T20:00:00Z' },
      ];
      await vi.advanceTimersByTimeAsync(15_000);
      expect(store.openListingsCount()).toBe(0);
      expect(fakeApi.listingsCalls).toBe(callsAfterSession + 2);
    } finally {
      vi.useRealTimers();
    }
  });

  it('dropping the stream flips the indicator and refetches; the shared loop owns the cadence', async () => {
    vi.useFakeTimers();
    try {
      fakeWallet.address.set(WALLET_ADDRESS);
      fakeWallet.accountId.set('0xmockacct');
      fakeWallet.token.set('mock-token');
      TestBed.flushEffects();
      await vi.advanceTimersByTimeAsync(0);

      // 1. Session established: socket created, not yet open.
      expect(store.streamState()).toBe('disconnected');
      expect(FakeEventSource.instances.length).toBe(1);
      const es = FakeEventSource.instances[0];
      expect(es.url.startsWith(`${environment.apiUrl}/console/stream?token=`)).toBe(true);

      // 2. Socket opens: connected.
      es.open();
      expect(store.streamState()).toBe('connected');

      // 3. Socket drops: indicator back to stale, one immediate refetch so
      //    the balance is not left waiting for the next ordinary tick.
      const accountCallsBefore = fakeApi.accountCalls;
      es.fail();
      expect(es.readyState).toBe(2);
      expect(store.streamState()).toBe('disconnected');
      await vi.advanceTimersByTimeAsync(0);
      expect(fakeApi.accountCalls).toBe(accountCallsBefore + 1);

      // 4. Backoff reconnect fires at 1 s and opens again. The 15 s account
      //    cadence is the shared loop's whether the stream is up or down, so
      //    no separate fallback timer exists to start or stop here.
      await vi.advanceTimersByTimeAsync(1_000);
      expect(FakeEventSource.instances.length).toBe(2);
      const es2 = FakeEventSource.instances[1];
      es2.open();
      expect(store.streamState()).toBe('connected');
    } finally {
      vi.useRealTimers();
    }
  });

  // ---- transactions (merged view) tests --------------------------------

  it('merges deposits and listings newest first, with the right kind and status wording', async () => {
    fakeApi.fundingListData = [
      makeFunding({ purchaseId: 'dep-settled', state: 'settled', credits: 250_000, startedAt: '2026-08-24T10:00:00Z' }),
      makeFunding({ purchaseId: 'dep-pending', state: 'confirmed', ethBlock: 100, startedAt: '2026-08-25T09:00:00Z' }),
      makeFunding({ purchaseId: 'dep-wire-pending', state: 'pending', startedAt: '2026-08-25T08:30:00Z' }),
    ];
    fakeApi.listingsData = [
      { id: 'b1', listedTx: '0xbuytx1', state: 'sold', role: 'buying', credits: 5_000, price: 500, token: 'mUSD1', matured: true, createdAt: '2026-08-25T08:00:00Z' },
      { id: 's-open-immature', listedTx: '0xlistoi', state: 'open', role: 'selling', credits: 5_000, price: 500, token: 'mUSD1', matured: false, createdAt: '2026-08-23T00:00:00Z' },
      { id: 's-open-matured', listedTx: '0xlistom', state: 'open', role: 'selling', credits: 5_000, price: 500, token: 'mUSD1', matured: true, createdAt: '2026-08-22T00:00:00Z' },
      { id: 's-sold', listedTx: '0xlistsold', state: 'sold', role: 'selling', credits: 5_000, price: 500, token: 'mUSD1', matured: true, createdAt: '2026-08-21T00:00:00Z' },
      { id: 's-cancelled', listedTx: '0xlistcx', state: 'cancelled', role: 'selling', credits: 5_000, price: 500, token: 'mUSD1', matured: false, createdAt: '2026-08-20T00:00:00Z' },
    ];
    await establishSession();
    await store.loadListings();

    const rows = store.transactions();
    expect(rows.map((r) => r.id)).toEqual([
      'dep-pending',
      'dep-wire-pending',
      'b1',
      'dep-settled',
      's-open-immature',
      's-open-matured',
      's-sold',
      's-cancelled',
    ]);

    const byId = new Map(rows.map((r) => [r.id, r]));
    expect(byId.get('dep-pending')?.kind).toBe('deposit');
    expect(byId.get('dep-pending')?.statusText).toBe('Proving');
    expect(byId.get('dep-wire-pending')?.statusText).toBe('Pending');
    expect(byId.get('dep-settled')?.kind).toBe('deposit');
    expect(byId.get('dep-settled')?.statusText).toBe('Settled');
    expect(byId.get('b1')?.kind).toBe('buy');
    expect(byId.get('b1')?.statusText).toBe('Bought');
    expect(byId.get('s-open-immature')?.kind).toBe('sell');
    expect(byId.get('s-open-immature')?.statusText).toBe('For sale');
    expect(byId.get('s-open-matured')?.kind).toBe('sell');
    expect(byId.get('s-open-matured')?.statusText).toBe('Matured');
    expect(byId.get('s-sold')?.statusText).toBe('Sold');
    expect(byId.get('s-cancelled')?.statusText).toBe('Cancelled');
    // The Network column reads the row's chain metadata: the wire chain
    // name for deposits, 'Solana' for market rows.
    expect(byId.get('dep-pending')?.chainName).toBe('Ethereum');
    expect(byId.get('dep-settled')?.chainName).toBe('Ethereum');
    expect(byId.get('b1')?.chainName).toBe('Solana');
    expect(byId.get('s-open-immature')?.chainName).toBe('Solana');
    expect(byId.get('s-sold')?.chainName).toBe('Solana');
  });

  it('renders listing prices scaled by the pay token decimals, with a raw marker when unknown', async () => {
    fakeApi.listingsData = [
      { id: 's1', listedTx: '0xlist1', state: 'open', role: 'selling', credits: 5_000, price: 5_000_000, token: 'mUSD1', tokenAddress: 'DemoUSDCmint11111111111111111111111111111111', tokenDecimals: 6, matured: true, createdAt: '2026-08-23T00:00:00Z' },
      { id: 's2', listedTx: '0xlist2', state: 'sold', role: 'selling', credits: 2_000, price: 5_000_000, token: '', tokenAddress: '0x1111222233334444555566667777888899990000', matured: true, createdAt: '2026-08-22T00:00:00Z' },
    ];
    await establishSession();
    await store.loadListings();

    const byId = new Map(store.transactions().map((r) => [r.id, r]));
    expect(byId.get('s1')?.priceText).toBe('5.00 mUSD1');
    const unknown = byId.get('s2')?.priceText;
    expect(unknown).toContain(' raw ');
    expect(unknown).toContain('0x1111...0000');
  });

  it('filters Buying to deposits + buying listings, Selling to selling listings, All to everything', async () => {
    fakeApi.fundingListData = [
      makeFunding({ purchaseId: 'dep1', state: 'settled', credits: 100, startedAt: '2026-08-24T10:00:00Z' }),
    ];
    fakeApi.listingsData = [
      { id: 'b1', listedTx: '0xbuytx1', state: 'sold', role: 'buying', credits: 5_000, price: 500, token: 'mUSD1', matured: true, createdAt: '2026-08-23T00:00:00Z' },
      { id: 's1', listedTx: '0xlist1', state: 'open', role: 'selling', credits: 5_000, price: 500, token: 'mUSD1', matured: false, createdAt: '2026-08-22T00:00:00Z' },
      { id: 's2', listedTx: '0xlist2', state: 'sold', role: 'selling', credits: 5_000, price: 500, token: 'mUSD1', matured: true, createdAt: '2026-08-21T00:00:00Z' },
    ];
    await establishSession();
    await store.loadListings();

    store.setTransactionFilter('all');
    expect(store.filteredTransactions().map((r) => r.id)).toEqual(['dep1', 'b1', 's1', 's2']);
    expect(store.transactionCount()).toBe(4);

    store.setTransactionFilter('buying');
    expect(store.filteredTransactions().map((r) => r.id)).toEqual(['dep1', 'b1']);
    expect(store.transactionCount()).toBe(2);

    store.setTransactionFilter('selling');
    expect(store.filteredTransactions().map((r) => r.id)).toEqual(['s1', 's2']);
    expect(store.transactionCount()).toBe(2);
  });

  it('gives each row its destination: deposit to trace, open sell to the sell screen, finished trades to the detail, open buy to the explorer link', async () => {
    fakeApi.fundingListData = [
      makeFunding({ purchaseId: 'dep1', state: 'settled', credits: 100, startedAt: '2026-08-24T10:00:00Z' }),
    ];
    fakeApi.listingsData = [
      { id: 'b1', listedTx: '0xbuytx1', state: 'sold', role: 'buying', credits: 5_000, price: 500, token: 'mUSD1', matured: true, createdAt: '2026-08-23T00:00:00Z' },
      { id: 's1', listedTx: '0xlist1', state: 'open', role: 'selling', credits: 5_000, price: 500, token: 'mUSD1', matured: false, createdAt: '2026-08-22T00:00:00Z' },
      { id: 's2', listedTx: '0xlist2', state: 'sold', role: 'selling', credits: 5_000, price: 500, token: 'mUSD1', matured: true, createdAt: '2026-08-21T00:00:00Z' },
      { id: 'c1', listedTx: '0xlist3', state: 'cancelled', role: 'selling', credits: 5_000, price: 500, token: 'mUSD1', matured: true, createdAt: '2026-08-20T00:00:00Z' },
      { id: 'bo', listedTx: '0xbuytxo', state: 'open', role: 'buying', credits: 5_000, price: 500, token: 'mUSD1', matured: true, createdAt: '2026-08-19T00:00:00Z' },
    ];
    await establishSession();
    await store.loadListings();

    const byId = new Map(store.transactions().map((r) => [r.id, r]));

    const dep = byId.get('dep1')!;
    expect(dep.destination).toEqual({ kind: 'route', path: ['/funding', 'dep1'] });

    // An open selling row stays on the Sell screen for management.
    const sell = byId.get('s1')!;
    expect(sell.destination).toEqual({ kind: 'route', path: ['/sell'] });

    // Finished trades (sold or cancelled, either role) open the detail view.
    const soldSell = byId.get('s2')!;
    expect(soldSell.destination).toEqual({ kind: 'route', path: ['/trade', 's2'] });
    const bought = byId.get('b1')!;
    expect(bought.destination).toEqual({ kind: 'route', path: ['/trade', 'b1'] });
    const cancelled = byId.get('c1')!;
    expect(cancelled.destination).toEqual({ kind: 'route', path: ['/trade', 'c1'] });

    // An open buying row keeps its external explorer link (the in-flight case).
    const openBuy = byId.get('bo')!;
    expect(openBuy.destination.kind).toBe('external');
    if (openBuy.destination.kind === 'external') {
      expect(openBuy.destination.href).toContain('explorer.solana.com');
      expect(openBuy.destination.href).toContain('/tx/0xbuytxo');
      expect(openBuy.destination.label).toBe('Solana explorer · transaction');
    }
  });

  // ---- payment source chains tests -------------------------------------

  it('loads the payment source chains and surfaces a read failure', async () => {
    fakeApi.chainsData = [
      { chainKey: 0, name: 'Solana', chainId: 0, deposit: 'DemoTreasuryTok1111111111111111111111111111111', explorer: 'https://explorer.solana.com' },
    ];
    await store.loadChains();
    expect(store.chains().length).toBe(1);
    expect(store.chains()[0].name).toBe('Solana');
    expect(store.chainsError()).toBeNull();

    // A failed read keeps the previous list and names the failure.
    fakeApi.failNext = new ApiError(500, 'internal', 'chains down');
    await store.loadChains();
    expect(store.chains().length).toBe(1);
    expect(store.chainsError()).toBe('chains down');
    store.clearChainsError();
    expect(store.chainsError()).toBeNull();
  });

  it('threads the trace source explorer from the purchase chain key', async () => {
    fakeApi.chainsData = [
      { chainKey: 0, name: 'Solana', chainId: 0, deposit: 'DemoTreasuryTok1111111111111111111111111111111', explorer: 'https://explorer.solana.com' },
    ];
    await store.loadChains();
    fakeApi.fundingListData = [makeFunding({ chainKey: 0, chainName: 'Solana', state: 'confirmed', ethBlock: 100 })];
    store.startPolling();
    await tick();
    expect(store.traceSourceExplorer()).toBe('https://explorer.solana.com');
    // The source lane link uses that base, not a hardcoded host.
    expect(store.steps()[0].linkHref).toContain('explorer.solana.com');

    // An unknown chain key leaves the base empty: the link stays blank
    // rather than pointing at a host nobody configured.
    fakeApi.fundingListData = [makeFunding({ chainKey: 99, state: 'confirmed', ethBlock: 100 })];
    store.startPolling();
    await tick();
    expect(store.traceSourceExplorer()).toBe('');
  });

  // ---- attestation health tests ------------------------------------------

  it('loadAttestation sets the rows and clears the error; a failure keeps the previous list', async () => {
    await establishSession();
    const fixture: AttestChain[] = [
      { chainKey: 1, name: 'Ethereum', known: true, attestedTip: 1_000_000, head: 1_000_001, lagBlocks: 1, lagSeconds: 12 },
      { chainKey: 3, name: 'Ethereum', known: false, attestedTip: 0, head: 0, lagBlocks: 0, lagSeconds: 0 },
    ];
    fakeApi.attestationData = fixture;
    store.attestationError.set('stale');
    await store.loadAttestation();
    expect(store.attestation()).toEqual(fixture);
    expect(store.attestationError()).toBeNull();
    expect(store.attestationInitial()).toBe(false);

    // A failed read leaves the previous list in place and surfaces the error.
    fakeApi.attestationFailNext = new ApiError(500, 'internal', 'feeder down');
    await store.loadAttestation();
    expect(store.attestationError()).toBe('feeder down');
    expect(store.attestation()).toEqual(fixture);
    // Dismiss brings the card back to its stale-without-warning state.
    store.clearAttestationError();
    expect(store.attestationError()).toBeNull();
  });

  it('a poll tick re-reads the attestation health', async () => {
    vi.useFakeTimers();
    try {
      await establishSessionFake();
      const calls = fakeApi.attestationCalls;
      await vi.advanceTimersByTimeAsync(15_000);
      expect(fakeApi.attestationCalls).toBe(calls + 1);
    } finally {
      vi.useRealTimers();
    }
  });

  // ---- model catalog (picker) tests ------------------------------------

  it('loads the catalog and picks the first model by default', async () => {
    await establishSession();
    expect(store.models().length).toBe(4);
    expect(store.selectedModel()?.id).toBe('mistral-nemotron');
    expect(store.cheapestModel()?.id).toBe('llama-3.2-3b');
  });

  it('switches the priced model when a different one is selected', async () => {
    await establishSession();
    store.setModel('qwen3-8-27b');
    expect(store.selectedModel()?.id).toBe('qwen3-8-27b');
  });

  it('keeps the previous catalog and surfaces an error when models() fails', async () => {
    await establishSession();
    const before = store.models();
    fakeApi.failNext = new ApiError(503, 'unavailable', 'nope');
    await store.loadModels();
    expect(store.modelsError()).not.toBeNull();
    // The stale list stays in place rather than blanking out.
    expect(store.models()).toEqual(before);
    expect(store.selectedModel()?.id).toBe('mistral-nemotron');
  });

  // ---- advertised base URL (EP-032-T04) ---------------------------------

  it('maps the models payload baseUrl into endpointBaseUrl, not the page origin', async () => {
    fakeApi.modelBaseUrl = 'https://wire.example/v1';
    await establishSession();
    expect(store.advertisedBaseUrl()).toBe('https://wire.example/v1');
    expect(store.endpointBaseUrl()).toBe('https://wire.example/v1');
    // A later account read without a baseUrl must not clobber it.
    fakeApi.accountBaseUrl = '';
    await store.loadAccount();
    expect(store.advertisedBaseUrl()).toBe('https://wire.example/v1');
    expect(store.endpointBaseUrl()).toBe('https://wire.example/v1');
  });

  it('keeps the page-origin fallback when the wire advertises nothing', async () => {
    expect(fakeApi.modelBaseUrl).toBe('');
    await establishSession();
    expect(store.advertisedBaseUrl()).toBe('');
    expect(store.endpointBaseUrl()).toBe(window.location.origin + '/v1');
  });

  it('leaves the signal untouched when a models payload carries no baseUrl', async () => {
    await establishSession();
    store.advertisedBaseUrl.set('https://pre.example/v1');
    // modelBaseUrl is '' by default: absent on the wire.
    await store.loadModels();
    expect(store.advertisedBaseUrl()).toBe('https://pre.example/v1');
  });

  it('still honors an account-wire baseUrl as a fallback when the models payload has none', async () => {
    fakeApi.accountBaseUrl = 'https://acct.example/v1';
    await establishSession();
    expect(store.advertisedBaseUrl()).toBe('https://acct.example/v1');
    expect(store.endpointBaseUrl()).toBe('https://acct.example/v1');
  });
});
