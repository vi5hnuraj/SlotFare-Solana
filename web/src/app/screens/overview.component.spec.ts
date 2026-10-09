import { computed, signal } from '@angular/core';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { provideRouter } from '@angular/router';
import { ConsoleStore } from '../core/console-store';
import { BreakpointService } from '../core/breakpoint';
import { ToastService } from '../core/toast';
import {
  Account, ApiError, AttestChain, ChainInfo,
  CreatedKey, DepositTarget, FundingStatus, Key,
  ListingPage, MarketConfirmBody, MarketPrepareBody, ModelList,
  Nonce, PreparedMarketTx, SLOTFARE_API, SlotfareApi,
  UsagePage, UsageProof, UsageQuery, VerifyResult,
} from '../core/api';
import { ConsoleSession, WalletService } from '../core/wallet';
import { Listing, MarketListing, Model, Wallet } from '../core/models';
import { creditText, num } from '../core/pricing';
import { environment } from '../../environments/environment';
import { OverviewComponent } from './overview.component';

const tick = (): Promise<void> => new Promise((r) => setTimeout(r));

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

/** The ".card" whose head reads as the given title. */
function cardByTitle(root: HTMLElement, title: string): HTMLElement {
  const card = Array.from(root.querySelectorAll<HTMLElement>('.card'))
    .find((c) => c.querySelector('h3')?.textContent?.trim() === title);
  expect(card).toBeTruthy();
  return card!;
}

function makeFakeWallet(): WalletService {
  const address = signal<string | null>(null);
  const chainId = signal<string | null>(null);
  const connecting = signal(false);
  const error = signal<string | null>(null);
  const accountId = signal<string | null>(null);
  const token = signal<string | null>(null);
  const session = computed<ConsoleSession | null>(() => {
    const t = token();
    if (t === null) return null;
    const a = address();
    const id = accountId();
    if (a === null || id === null) return null;
    return { address: a, accountId: id, token: t };
  });
  return {
    providers: signal<Wallet[]>([]),
    address,
    chainId,
    connecting,
    error,
    accountId,
    token,
    session,
    provider: null,
    connect: () => undefined,
    startSession: () => undefined,
    clearSession: () => {
      token.set(null);
      accountId.set(null);
      error.set(null);
    },
  } as unknown as WalletService;
}

/** A brand-new account: zero balance and no usage at all. */
class EmptyAccountApi implements SlotfareApi {
  /** Mutable locked figure; an ordinary refresh-cycle read picks it up. */
  lockedCredits = 0;
  /** Mutable list returned by listings(). */
  listingsData: Listing[] = [];
  /** Mutable balance returned by account(); tests can change it between reads. */
  accountBalance = 0;
  /** When true, the next account() delays ~20 ms and resets the flag. */
  slowNextAccount = false;
  /** When set, the next account() rejects with it once. */
  failNextAccount: ApiError | null = null;
  /** Mutable rows returned by fundingList(); the overview funding card
   *  renders one of these when non-empty. */
  fundingListData: FundingStatus[] = [];
  /** Mutable rows returned by attestation(); the attestation card renders
   *  one row per entry and hides itself when the list is empty. */
  attestationData: AttestChain[] = [];

  async nonce(_address: string): Promise<Nonce> {
    return { nonce: 'n', statement: 's', issuedAt: '', expiresAt: '' };
  }
  async verify(
    _a: string,
    _n: string,
    _s: string,
  ): Promise<VerifyResult> {
    return { accountId: '0xmockacct', token: 't', expiresAt: '' };
  }
  async account(): Promise<Account> {
    if (this.failNextAccount !== null) {
      const e = this.failNextAccount;
      this.failNextAccount = null;
      throw e;
    }
    let gate: Promise<void> = Promise.resolve();
    if (this.slowNextAccount) {
      this.slowNextAccount = false;
      gate = new Promise((r) => setTimeout(r, 20));
    }
    return gate.then(() => ({
      accountId: '0xmockacct',
      address: '0xabc',
      balanceCredits: this.accountBalance,
      lockedCredits: this.lockedCredits,
      totalSpentCredits: 0,
      createdAt: '2026-06-14',
      baseUrl: '',
    }));
  }
  async keys(): Promise<Key[]> {
    return [];
  }
  async createKey(_label: string): Promise<CreatedKey> {
    throw new ApiError(500, 'internal', 'no');
  }
  async revokeKey(_id: string): Promise<void> {}
  async usage(_query?: UsageQuery): Promise<UsagePage> {
    return { rows: [], nextCursor: null };
  }
  async funding(_purchaseId: string): Promise<FundingStatus> {
    throw new ApiError(500, 'internal', 'no');
  }
  async fundingList(): Promise<FundingStatus[]> {
    return this.fundingListData.map((s) => ({ ...s }));
  }
  async listings(_before?: string): Promise<ListingPage> {
    return { rows: this.listingsData.map((l) => ({ ...l })), nextCursor: null };
  }
  async market(): Promise<MarketListing[]> {
    return [];
  }
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
    return [];
  }
  async attestation(): Promise<AttestChain[]> {
    return this.attestationData.map((c) => ({ ...c }));
  }
  /** The demo catalog: the overview's token estimates price on its cheapest model. */
  async models(): Promise<ModelList> {
    return {
      models: [
        { id: 'mistral-nemotron', name: 'Mistral Nemotron', pricing: { uncachedInput: 20, cachedInput: 2, output: 30 } },
        { id: 'diffusiongemma-26b', name: 'Google DiffusionGemma 26B A4B', pricing: { uncachedInput: 50, cachedInput: 5, output: 250 } },
        { id: 'llama-3.2-3b', name: 'Llama 3.2 3B', pricing: { uncachedInput: 10, cachedInput: 1, output: 40 } },
        { id: 'qwen3-8-27b', name: 'Qwen3 27B', pricing: { uncachedInput: 100, cachedInput: 10, output: 300 } },
      ],
      baseUrl: '',
    };
  }
  /** EP-027-T05: the inclusion proof read; this screen does not exercise it. */
  async usageProof(_rowId: string): Promise<UsageProof> {
    throw new ApiError(500, 'internal', 'not implemented');
  }
}

describe('OverviewComponent', () => {
  // The store's stream gate is environment.useMock; keep it true so
  // these specs never open a real EventSource (jsdom has none).
  beforeAll(() => {
    environment.useMock = true;
  });
  let fixture: ComponentFixture<OverviewComponent>;
  let fakeWallet: WalletService;

  function setup() {
    fakeWallet = makeFakeWallet();
    TestBed.configureTestingModule({
      imports: [OverviewComponent],
      providers: [
        provideRouter([]),
        { provide: SLOTFARE_API, useValue: new EmptyAccountApi() },
        { provide: WalletService, useValue: fakeWallet },
        ConsoleStore,
        ToastService,
      ],
    });
    fixture = TestBed.createComponent(OverviewComponent);
    fixture.detectChanges();
    fakeWallet.address.set('0xabc');
    fakeWallet.accountId.set('0xmockacct');
    fakeWallet.token.set('mock-token');
    TestBed.flushEffects();
  }

  afterEach(() => TestBed.inject(ConsoleStore).stopPolling());

  function configure(api: EmptyAccountApi) {
    fakeWallet = makeFakeWallet();
    TestBed.configureTestingModule({
      imports: [OverviewComponent],
      providers: [
        provideRouter([]),
        { provide: SLOTFARE_API, useValue: api },
        { provide: WalletService, useValue: fakeWallet },
        ConsoleStore,
        ToastService,
      ],
    });
    fixture = TestBed.createComponent(OverviewComponent);
    fixture.detectChanges();
    fakeWallet.address.set('0xabc');
    fakeWallet.accountId.set('0xmockacct');
    fakeWallet.token.set('mock-token');
    TestBed.flushEffects();
  }

  it('renders skeletons while the first load is in flight and values once it settles', async () => {
    configure(new EmptyAccountApi());
    // The first fetches are still in flight: the skeleton is up.
    fixture.detectChanges();
    expect(fixture.nativeElement.querySelector('slf-skeleton')).not.toBeNull();

    await tick();
    fixture.detectChanges();
    // Everything settled: no skeleton remains, the figures are on screen.
    expect(fixture.nativeElement.querySelectorAll('slf-skeleton').length).toBe(0);
    expect(fixture.nativeElement.outerHTML).toContain(num(0));
  });

  it('keeps the balance figure on screen through a background refresh', async () => {
    const api = new EmptyAccountApi();
    api.accountBalance = 2_500;
    configure(api);
    await tick();
    fixture.detectChanges();
    expect(fixture.nativeElement.outerHTML).toContain(num(2_500));

    // The refresh takes ~20 ms; while it is in flight the rendered figure
    // must not tear down to a skeleton.
    api.slowNextAccount = true;
    const store = TestBed.inject(ConsoleStore);
    void store.loadAccount();
    fixture.detectChanges();
    expect(fixture.nativeElement.querySelectorAll('slf-skeleton').length).toBe(0);
    expect(fixture.nativeElement.outerHTML).toContain(num(2_500));

    await new Promise((r) => setTimeout(r, 30));
    fixture.detectChanges();
    expect(fixture.nativeElement.querySelectorAll('slf-skeleton').length).toBe(0);
    expect(fixture.nativeElement.outerHTML).toContain(num(2_500));
  });

  it('keeps the stale balance on screen with the error visible when a refresh fails', async () => {
    const api = new EmptyAccountApi();
    api.accountBalance = 2_500;
    configure(api);
    await tick();
    fixture.detectChanges();

    api.failNextAccount = new ApiError(500, 'internal', 'boom');
    const store = TestBed.inject(ConsoleStore);
    await store.loadAccount();
    fixture.detectChanges();

    const html = fixture.nativeElement.outerHTML;
    expect(html).toContain(num(2_500));
    expect(html).toContain('boom');
    expect(fixture.nativeElement.querySelectorAll('slf-skeleton').length).toBe(0);
  });

  it('shows "Not enough usage yet" for a new account with no usage', async () => {
    setup();
    await tick();
    fixture.detectChanges();
    expect(fixture.nativeElement.outerHTML).toContain('Not enough usage yet');
  });

  it('never renders NaN or Infinity on a screen with no usage', async () => {
    setup();
    await tick();
    fixture.detectChanges();
    const html = fixture.nativeElement.outerHTML;
    expect(html).not.toContain('NaN');
    expect(html).not.toContain('Infinity');
  });

  it('shows the source chain badge on the latest-funding card', async () => {
    setup();
    // Let the initial poll settle (empty list stops polling) so a later
    // fundingStatus set is not clobbered by an in-flight poll.
    await tick();
    const store = TestBed.inject(ConsoleStore);
    store.fundingStatus.set({
      purchaseId: 'p1',
      state: 'confirmed',
      ethTx: '0x9f2ad4e1c88b7605a3f0d2914e77bb60c4a1de0938f5c2b7a04e61d3be04c115',
      ethBlock: 100,
      attestedHeight: null,
      chainTip: 150,
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
    });
    fixture.detectChanges();
    expect(fixture.nativeElement.outerHTML).toContain('Ethereum');
  });

  it('a locked change arriving in the ordinary refresh cycle updates the overview card without navigation', async () => {
    const api = new EmptyAccountApi();
    fakeWallet = makeFakeWallet();
    TestBed.configureTestingModule({
      imports: [OverviewComponent],
      providers: [
        provideRouter([]),
        { provide: SLOTFARE_API, useValue: api },
        { provide: WalletService, useValue: fakeWallet },
        ConsoleStore,
        ToastService,
      ],
    });
    const f = TestBed.createComponent(OverviewComponent);
    f.detectChanges();
    fakeWallet.address.set('0xabc');
    fakeWallet.accountId.set('0xmockacct');
    fakeWallet.token.set('mock-token');
    TestBed.flushEffects();
    await tick();
    f.detectChanges();

    // No lock yet: the card is absent.
    expect(f.nativeElement.outerHTML).not.toContain('Credits locked');

    // A buyer takes a listing elsewhere; the ordinary refresh-cycle read
    // (no navigation involved) surfaces the new figure on the card.
    api.lockedCredits = 5_000;
    const store = TestBed.inject(ConsoleStore);
    await store.loadAccount();
    f.detectChanges();
    expect(store.locked()).toBe(5_000);
    expect(f.nativeElement.outerHTML).toContain('Credits locked');
    expect(f.nativeElement.outerHTML).toContain(num(5_000));
  });

  it('shows an in-progress buy card that flips to complete when the mirror lands the buying row', async () => {
    const api = new EmptyAccountApi();
    fakeWallet = makeFakeWallet();
    TestBed.configureTestingModule({
      imports: [OverviewComponent],
      providers: [
        provideRouter([]),
        { provide: SLOTFARE_API, useValue: api },
        { provide: WalletService, useValue: fakeWallet },
        ConsoleStore,
        ToastService,
      ],
    });
    const f = TestBed.createComponent(OverviewComponent);
    f.detectChanges();
    fakeWallet.address.set('0xabc');
    fakeWallet.accountId.set('0xmockacct');
    fakeWallet.token.set('mock-token');
    TestBed.flushEffects();
    const store = TestBed.inject(ConsoleStore);

    // A market buy that has settled: the card lands complete, because the
    // buy call returns only after the server moved the credits.
    store.announceBuy('0xbuytx1', 5_000, '5.00 USDC');
    TestBed.flushEffects();
    await tick();
    f.detectChanges();
    let html = f.nativeElement.outerHTML;
    expect(html).toContain('Buy in progress');
    expect(html).toContain('Credited');
    expect(html).toContain(num(5_000));
    expect(store.buyCard()?.complete).toBe(true);

    // The mirror read keeps it complete: the buying row the server returns
    // under the same listedTx reconciles the card instead of flipping it.
    api.listingsData = [
      { id: '7', listedTx: '0xbuytx1', state: 'sold', role: 'buying', credits: 5_000, price: 500, token: 'mUSD1', matured: true, createdAt: '2026-08-22T20:00:00Z' },
    ];
    await store.loadListings();
    f.detectChanges();
    html = f.nativeElement.outerHTML;
    expect(html).toContain('Credited');
    expect(store.buyCard()?.complete).toBe(true);
  });

  it('describes the deposit flow when no funding rows exist, with no dead links', async () => {
    setup();
    await tick();
    fixture.detectChanges();
    const card = cardByTitle(fixture.nativeElement, 'Funding in progress');
    const html = card.outerHTML;
    // The honest empty state describes the on-chain deposit, no purchase CTA.
    expect(html).toContain('No funding in progress');
    expect(html).toContain('Deposit USDC to the treasury account');
    const cta = Array.from(card.querySelectorAll<HTMLAnchorElement>('a.btn')).find(
      (a) => a.textContent?.trim() === 'Purchase credits',
    );
    expect(cta).toBeUndefined();
    expect(html).not.toContain('routerlink="/add"');
    // Fixture-shaped content must never render.
    expect(html).not.toContain('Watch the trace');
    expect(html).not.toContain('Proving');
    expect(html).not.toContain('250.00 mUSDC');
    expect(html).not.toContain(creditText(250_000));
    expect(card.querySelector('.trace-steps-bar')).toBeNull();
  });

  it('renders a settled funding row as today, without the empty state', async () => {
    const api = new EmptyAccountApi();
    api.fundingListData = [makeFunding({ purchaseId: 'dep1', state: 'settled', credits: 250_000 })];
    configure(api);
    await tick();
    fixture.detectChanges();
    const card = cardByTitle(fixture.nativeElement, 'Funding in progress');
    const html = card.outerHTML;
    expect(html).toContain('Settled');
    expect(html).toContain('Ethereum');
    expect(html).toContain('Watch the trace');
    expect(card.querySelectorAll('.trace-steps-bar i').length).toBe(3);
    expect(html).not.toContain('No funding in progress');
    expect(html).not.toContain('Purchase credits');
  });

  it('shows the attestation card with lag phrasing, grades, and an unreachable row', async () => {
    const api = new EmptyAccountApi();
    api.attestationData = [
      { chainKey: 1, name: 'Ethereum', known: true, attestedTip: 1_000_000, head: 1_000_001, lagBlocks: 1, lagSeconds: 38 },
      { chainKey: 3, name: 'Ethereum', known: true, attestedTip: 5_000_000, head: 5_000_030, lagBlocks: 12, lagSeconds: 0 },
      { chainKey: 4, name: 'Base', known: false, attestedTip: 0, head: 0, lagBlocks: 0, lagSeconds: 0 },
    ];
    configure(api);
    await tick();
    fixture.detectChanges();
    const card = cardByTitle(fixture.nativeElement, 'Attestation');
    const html = card.outerHTML;
    // Seconds lag phrased for humans; block fallback when seconds are undetermined.
    expect(html).toContain('38 s behind');
    expect(html).toContain('12 blocks behind');
    // An unknown read says unreachable, honestly.
    expect(html).toContain('unreachable');
    // The grade pills carry their colors: 38 s is past the 30 s normal
    // threshold, so it reads slate; unreachable reads neutral.
    expect(card.querySelector('.pill-live')).not.toBeNull();
    expect(card.querySelector('.pill-wait')).not.toBeNull();
  });

  it('grades the attestation rows at the thresholds in the card', async () => {
    const api = new EmptyAccountApi();
    api.attestationData = [
      { chainKey: 1, name: 'Ethereum', known: true, attestedTip: 1_000_000, head: 1_000_001, lagBlocks: 0, lagSeconds: 12 },
      { chainKey: 3, name: 'Ethereum', known: true, attestedTip: 5_000_000, head: 5_000_030, lagBlocks: 30, lagSeconds: 480 },
    ];
    configure(api);
    await tick();
    fixture.detectChanges();
    const card = cardByTitle(fixture.nativeElement, 'Attestation');
    // Normal (<= 30 s) reads teal; stalled (> 300 s) reads gold.
    expect(card.querySelector('.pill-ok')).not.toBeNull();
    expect(card.querySelector('.pill-alert')).not.toBeNull();
    expect(card.querySelector('.pill-live')).toBeNull();
  });

  it('hides the attestation card when the wire reports no source chains', async () => {
    setup();
    await tick();
    fixture.detectChanges();
    // A Solana-only deployment: no card at all, not even a heading.
    expect(fixture.nativeElement.outerHTML).not.toContain('>Attestation<');
  });

  it('renders a pending funding row live, not as the empty state', async () => {
    const api = new EmptyAccountApi();
    api.fundingListData = [makeFunding({ purchaseId: 'dep1', state: 'pending' })];
    configure(api);
    await tick();
    fixture.detectChanges();
    const card = cardByTitle(fixture.nativeElement, 'Funding in progress');
    const html = card.outerHTML;
    expect(html).toContain('Proving');
    expect(html).toContain('Watch the trace');
    expect(html).not.toContain('No funding in progress');
    expect(html).not.toContain('Purchase credits');
  });

  it('shows a skeleton before the first funding poll settles and an error block on failure', async () => {
    const api = new EmptyAccountApi();
    api.fundingListData = [makeFunding({ state: 'pending' })];
    configure(api);
    // The first poll is still in flight: the card shows its skeleton.
    expect(cardByTitle(fixture.nativeElement, 'Funding in progress')
      .querySelector('slf-skeleton')).not.toBeNull();

    await tick();
    fixture.detectChanges();
    const store = TestBed.inject(ConsoleStore);
    store.fundingError.set('poll failed');
    fixture.detectChanges();
    const card = cardByTitle(fixture.nativeElement, 'Funding in progress');
    expect(card.querySelector('slf-error-block')).not.toBeNull();
    expect(card.outerHTML).toContain('poll failed');
  });

  it('collapses the card split to one column on mobile, two on tablet and desktop', () => {
    setup();
    const split = fixture.nativeElement.querySelector('.split') as HTMLElement;
    expect(split).toBeTruthy();

    TestBed.inject(BreakpointService).setForTest('mobile', true);
    fixture.detectChanges();
    expect(split.classList).toContain('cols-1');

    TestBed.inject(BreakpointService).setForTest('tablet', true);
    fixture.detectChanges();
    expect(split.classList).not.toContain('cols-1');

    TestBed.inject(BreakpointService).setForTest('desktop', false);
    fixture.detectChanges();
    expect(split.classList).not.toContain('cols-1');

    // Structural overlap guards: two grid stacks, no absolutely positioned or
    // fixed-width descendants anywhere inside the split.
    const children = Array.from(split.children);
    expect(children.length).toBe(2);
    expect(children.every((c) => c.classList.contains('grid'))).toBe(true);
    for (const el of Array.from(split.querySelectorAll<HTMLElement>('*'))) {
      expect(el.style.position).not.toBe('absolute');
      expect(el.style.position).not.toBe('fixed');
      expect(el.style.width).toBe('');
    }
  });
});
