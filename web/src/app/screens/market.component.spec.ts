import { computed, signal } from '@angular/core';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { Router } from '@angular/router';
import { ConsoleStore } from '../core/console-store';
import { BuyService, BUY_ERRORS } from '../core/buy';
import { SellService, SELL_ERRORS } from '../core/sell';
import { ToastService } from '../core/toast';
import {
  Account, ApiError, AttestChain, ChainInfo,
  CreatedKey, DepositTarget, FundingStatus, Key,
  ListingPage, MarketConfirmBody, MarketPrepareBody, ModelList,
  Nonce, PreparedMarketTx, SLOTFARE_API, SlotfareApi,
  UsagePage, UsageProof, UsageQuery, VerifyResult,
} from '../core/api';
import { Listing, MarketListing, Model, Wallet } from '../core/models';
import { environment } from '../../environments/environment';
import { ConsoleSession, WalletService } from '../core/wallet';
import { OPENING_BALANCE, OPENING_LOCKED } from '../core/mock-data';
import { MarketComponent } from './market.component';

const tick = (): Promise<void> => new Promise((r) => setTimeout(r));

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

class FakeApi implements SlotfareApi {
  marketCalls = 0;
  marketData: MarketListing[] = [];
  accountBalance = OPENING_BALANCE;
  async nonce(_a: string): Promise<Nonce> {
    return { nonce: 'n', statement: 's', issuedAt: '', expiresAt: '' };
  }
  async verify(_a: string, _n: string, _s: string): Promise<VerifyResult> {
    return { accountId: '0xmockacct', token: 't', expiresAt: '' };
  }
  async account(): Promise<Account> {
    return {
      accountId: '0xmockacct',
      address: '0xabc',
      balanceCredits: this.accountBalance,
      lockedCredits: OPENING_LOCKED,
      totalSpentCredits: 0,
      createdAt: '2026-06-14',
      baseUrl: '',
    };
  }
  async keys(): Promise<Key[]> {
    return [];
  }
  async createKey(_l: string): Promise<CreatedKey> {
    throw new ApiError(500, 'internal', 'no');
  }
  async revokeKey(_id: string): Promise<void> {}
  async usage(_q?: UsageQuery): Promise<UsagePage> {
    return { rows: [], nextCursor: null };
  }
  async funding(_id: string): Promise<FundingStatus> {
    throw new ApiError(500, 'internal', 'no');
  }
  async fundingList(): Promise<FundingStatus[]> {
    return [];
  }
  async listings(_before?: string): Promise<ListingPage> {
    return { rows: [], nextCursor: null };
  }
  async market(): Promise<MarketListing[]> {
    this.marketCalls += 1;
    return this.marketData.map((l) => ({ ...l }));
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
    return [];
  }
  async models(): Promise<ModelList> {
    return { models: [], baseUrl: '' };
  }

  /** EP-027-T05: the inclusion proof read; this screen does not exercise it. */
  async usageProof(_rowId: string): Promise<UsageProof> {
    throw new ApiError(500, 'internal', 'not implemented');
  }
}

describe('MarketComponent', () => {
  // The store's stream gate is environment.useMock; keep it true so
  // these specs never open a real EventSource (jsdom has none).
  beforeAll(() => {
    environment.useMock = true;
  });
  let fixture: ComponentFixture<MarketComponent>;
  let fakeApi: FakeApi;
  let fakeWallet: WalletService;
  let store: ConsoleStore;
  let buySpy: {
    calls: Array<[string, number, string, string | undefined]>;
    listingGone: ReturnType<typeof signal<boolean>>;
    error: ReturnType<typeof signal<string | null>>;
    step: ReturnType<typeof signal<string>>;
    rejectNext: boolean;
    rejectNextReceipt: boolean;
  };
  let cancelSpy: string[];
  /** The SellService stand-in's signals, so tests can drive rejection and
   *  pending states. */
  let sellFake: {
    step: ReturnType<typeof signal<string>>;
    error: ReturnType<typeof signal<string | null>>;
  };

  function setup() {
    fakeApi = new FakeApi();
    fakeWallet = makeFakeWallet();
    buySpy = {
      calls: [],
      listingGone: signal(false),
      error: signal<string | null>(null),
      step: signal('idle'),
      rejectNext: false,
      rejectNextReceipt: false,
    };
    cancelSpy = [];
    sellFake = { step: signal('idle'), error: signal<string | null>(null) };
    TestBed.configureTestingModule({
      imports: [MarketComponent],
      providers: [
        { provide: SLOTFARE_API, useValue: fakeApi },
        { provide: WalletService, useValue: fakeWallet },
        {
          provide: BuyService,
          useValue: {
            step: buySpy.step,
            error: buySpy.error,
            txHash: signal<string | null>(null),
            listingGone: buySpy.listingGone,
            busy: computed(() => buySpy.step() !== 'idle' && buySpy.step() !== 'done'),
            buy: async (l: {
              id: string;
              price: number;
              token: string;
              seller?: string;
            }): Promise<string> => {
              buySpy.calls.push([l.id, l.price, l.token, l.seller]);
              if (buySpy.rejectNext) {
                buySpy.rejectNext = false;
                throw new Error('bought by another');
              }
              if (buySpy.rejectNextReceipt) {
                buySpy.rejectNextReceipt = false;
                const msg =
                  'The purchase was reverted on-chain: nothing was paid and no credits moved.';
                buySpy.error.set(msg);
                throw new Error(msg);
              }
              return '0xbuytx00000000000000000000000000000000000000000000000000cd';
            },
            dismissError: () => {
              buySpy.error.set(null);
              buySpy.step.set('idle');
              buySpy.listingGone.set(false);
            },
          },
        },
        {
          provide: SellService,
          useValue: {
            step: sellFake.step,
            error: sellFake.error,
            txHash: signal<string | null>(null),
            busy: computed(() => sellFake.step() !== 'idle' && sellFake.step() !== 'done'),
            list: async (credits: number, symbol: string, priceUsd: number): Promise<Listing> => ({
              id: '0xlistingtx000000000000000000000000000000000000000000000000aa',
              listedTx: '0xlistingtx000000000000000000000000000000000000000000000000aa',
              state: 'open',
              role: 'selling',
              credits,
              price: Math.max(1, Math.round(priceUsd * 1e6)),
              token: symbol,
              tokenAddress: 'DemoUSDCmint11111111111111111111111111111111',
              tokenDecimals: 6,
              matured: false,
              createdAt: '2026-08-22T20:00:00Z',
            }),
            cancel: async (id: string): Promise<string> => {
              cancelSpy.push(id);
              return '0xcanceltx000000000000000000000000000000000000000000000000bb';
            },
            dismissError: () => {
              sellFake.error.set(null);
              sellFake.step.set('idle');
            },
          },
        },
        ConsoleStore,
        ToastService,
        {
          provide: Router,
          useValue: {
            navigate: (p: unknown[]) => {
              navigateSpy.push(p as string[]);
              return Promise.resolve(true);
            },
          },
        },
      ],
    });
    fixture = TestBed.createComponent(MarketComponent);
    store = TestBed.inject(ConsoleStore);
    fixture.detectChanges();
    // Establish a session so the store loads the account.
    fakeWallet.address.set('0xabc');
    fakeWallet.accountId.set('0xmockacct');
    fakeWallet.token.set('mock-token');
    TestBed.flushEffects();
  }

  const navigateSpy: string[][] = [];

  afterEach(() => {
    navigateSpy.length = 0;
    store.stopPolling();
    // Hosting the sell flow starts the bounded confirm loop on a listing;
    // it must not outlive the test.
    store.stopListingConfirm();
  });

  function root(): HTMLElement {
    return fixture.nativeElement as unknown as HTMLElement;
  }

  it('a refresh with open listings on screen never renders the skeleton', async () => {
    setup();
    fakeApi.marketData = [
      { id: '12', credits: 5_000, price: 500, token: 'mUSD1', maturesAt: 1756000000, matured: true, mine: false },
      { id: '9', credits: 10_000, price: 1_000, token: 'mUSD1', maturesAt: 1756000000, matured: true, mine: false },
    ];
    await store.loadMarket();
    fixture.detectChanges();
    expect(rows().length).toBe(2);

    // A background refresh: the rows stay on screen, no skeleton flashes in.
    void store.loadMarket();
    fixture.detectChanges();
    expect(root().querySelectorAll('slf-skeleton').length).toBe(0);
    expect(rows().length).toBe(2);

    await tick();
    fixture.detectChanges();
    expect(root().querySelectorAll('slf-skeleton').length).toBe(0);
    expect(rows().length).toBe(2);
  });

  it('the first market read renders the skeleton, then the rows', async () => {
    setup();
    // The constructor fired loadMarket with an empty market: before any tick
    // the read is in flight and no data exists, so the skeleton is up.
    fixture.detectChanges();
    expect(root().querySelectorAll('slf-skeleton').length).toBeGreaterThan(0);

    fakeApi.marketData = [
      { id: '12', credits: 5_000, price: 500, token: 'mUSD1', maturesAt: 1756000000, matured: true, mine: false },
    ];
    await store.loadMarket();
    fixture.detectChanges();
    expect(root().querySelectorAll('slf-skeleton').length).toBe(0);
    expect(rows().length).toBe(1);
  });

  function rows(): NodeListOf<HTMLTableRowElement> {
    return root().querySelectorAll('tbody tr');
  }

  function buyButtonForRow(i: number): HTMLButtonElement | null {
    const tr = rows()[i];
    if (!tr) return null;
    const b = tr.querySelector('button');
    return b as HTMLButtonElement | null;
  }

  it('renders a matured and an unmatured foreign row distinguishably; only the matured one is buyable', async () => {
    setup();
    // 3 h 30 min out: the hint floors to "3 h" and the gap cannot drift
    //  back into the 2 h bucket between the fixture and the component's now.
    const inThreeAndAHalf = Math.floor(Date.now() / 1000) + 3 * 3600 + 1800;
    fakeApi.marketData = [
      {
        id: '12',
        credits: 5_000,
        price: 500,
        token: 'mUSD1',
        maturesAt: 1756000000,
        matured: true,
        mine: false,
      },
      {
        id: '9',
        credits: 10_000,
        price: 1_000,
        token: 'mUSD1',
        maturesAt: inThreeAndAHalf,
        matured: false,
        mine: false,
      },
    ];
    await store.loadMarket();
    fixture.detectChanges();

    expect(rows().length).toBe(2);
    // Matured row: the pill says so and its Buy is enabled.
    expect(rows()[0].textContent).toContain('Matured');
    const maturedBtn = buyButtonForRow(0);
    expect(maturedBtn).not.toBeNull();
    expect((maturedBtn as HTMLButtonElement).disabled).toBe(false);
    // Unmatured row: visible but not yet buyable; it says when it matures
    // from `maturesAt` instead of hiding or enabling it early.
    expect(rows()[1].textContent).toContain('Not mature yet');
    expect(rows()[1].textContent).toContain('Matures in 3 h');
    const immatureBtn = buyButtonForRow(1);
    expect(immatureBtn).not.toBeNull();
    expect((immatureBtn as HTMLButtonElement).disabled).toBe(true);
  });

  it('renders market prices scaled by the pay token decimals, and a shortened address when the symbol is absent', async () => {
    setup();
    const mUSD1 = { address: 'DemoUSDCmint11111111111111111111111111111111' };
    const unknown = '0x1111222233334444555566667777888899990000';
    fakeApi.marketData = [
      {
        id: '12',
        credits: 5_000,
        price: 5_000_000,
        token: 'mUSD1',
        tokenAddress: mUSD1.address,
        tokenDecimals: 6,
        maturesAt: 1756000000,
        matured: true,
        mine: false,
      },
      {
        id: '9',
        credits: 10_000,
        price: 2_000_000,
        token: '',
        tokenAddress: unknown,
        maturesAt: 1756000000,
        matured: true,
        mine: false,
      },
      {
        id: '3',
        credits: 5_000,
        price: 5_000_000,
        token: '0x81A4C39b',
        tokenAddress: mUSD1.address,
        tokenDecimals: 0,
        maturesAt: 1756000000,
        matured: true,
        mine: false,
      },
    ];
    await store.loadMarket();
    fixture.detectChanges();

    // Known token: the raw wire amount is scaled and labelled with the symbol.
    const first = rows()[0];
    expect(first.textContent).toContain('5.00');
    expect(first.textContent).toContain('mUSD1');
    expect(first.textContent).not.toContain('5,000,000.00');

    // Symbol absent: the address shows in standard shortened form, never as
    // a bare truncated hex fragment; the unknown decimals get the raw marker.
    const second = rows()[1];
    expect(second.textContent).toContain('2,000,000.00 raw');
    expect(second.textContent).toContain('0x1111...0000');
    expect(second.textContent).not.toContain('0x11112222');

    // A bug-era row carries a shortened address in the symbol field and zero
    // decimals: the junk symbol is dropped, the label falls back to the
    // shortened pay-token address, and the raw marker stays honest - never
    // a fabricated money figure.
    const third = rows()[2];
    expect(third.textContent).toContain('5,000,000.00 raw');
    expect(third.textContent).toContain('Demo...1111');
    expect(third.textContent).not.toContain('0x81A4C39b');
  });

  it('renders an immature listing disabled with the maturity hint, and flips on the server boolean', async () => {
    setup();
    fakeApi.marketData = [
      {
        id: '9',
        credits: 10_000,
        price: 1_000,
        token: 'mUSD1',
        maturesAt: Math.floor(Date.now() / 1000) + 3 * 3600,
        matured: false,
        mine: false,
      },
    ];
    await store.loadMarket();
    fixture.detectChanges();

    // Immature: the Buy button is disabled and the maturity text says so.
    expect(rows().length).toBe(1);
    expect(root().textContent).toContain('Not mature yet');
    const btn = buyButtonForRow(0);
    expect(btn).not.toBeNull();
    expect((btn as HTMLButtonElement).disabled).toBe(true);

    // The server flips it mature; the same row's Buy becomes enabled.
    fakeApi.marketData[0] = { ...fakeApi.marketData[0], matured: true };
    await store.loadMarket();
    fixture.detectChanges();
    expect(root().textContent).toContain('Matured');
    expect(buyButtonForRow(0)).not.toBeNull();
    expect((buyButtonForRow(0) as HTMLButtonElement).disabled).toBe(false);
  });

  it('renders a mine listing marked and unbuyable', async () => {
    setup();
    fakeApi.marketData = [
      {
        id: '3',
        credits: 2_000,
        price: 200,
        token: 'mUSD1',
        maturesAt: 1755900000,
        matured: true,
        mine: true,
      },
    ];
    await store.loadMarket();
    fixture.detectChanges();

    expect(root().textContent).toContain('Yours');
    // No Buy button at all for a mine row.
    const tr = rows()[0];
    const buttons = Array.from(tr.querySelectorAll('button'));
    expect(buttons.some((b) => b.textContent?.trim() === 'Buy')).toBe(false);

    // The action cell offers Cancel instead of duplicating the seller badge:
    // exactly one Cancel, and 'Yours' appears once in the whole row.
    expect(buttons.filter((b) => b.textContent?.trim() === 'Cancel')).toHaveLength(1);
    expect((tr.textContent?.match(/Yours/g) ?? []).length).toBe(1);

    // Even if clicked through, the buy service is never called for it.
    expect(buySpy.calls.length).toBe(0);
  });

  it('counts one open own matured listing as 1 for sale and 0 buyable for you', async () => {
    setup();
    fakeApi.marketData = [
      {
        id: '3',
        credits: 2_000,
        price: 200,
        token: 'mUSD1',
        maturesAt: 1755900000,
        matured: true,
        mine: true,
      },
    ];
    await store.loadMarket();
    fixture.detectChanges();

    const eyebrow = root().querySelector('.eyebrow');
    expect(eyebrow).not.toBeNull();
    expect(eyebrow!.textContent).toContain('1 for sale');
    expect(eyebrow!.textContent).toContain('0 buyable for you');
  });

  it('counts a second account\'s matured listing as both 1', async () => {
    setup();
    fakeApi.marketData = [
      {
        id: '12',
        credits: 5_000,
        price: 500,
        token: 'mUSD1',
        maturesAt: 1756000000,
        matured: true,
        mine: false,
      },
    ];
    await store.loadMarket();
    fixture.detectChanges();

    // Both figures agree: the single-figure eyebrow stays.
    const eyebrows = Array.from(root().querySelectorAll('.eyebrow'));
    expect(eyebrows.length).toBe(1);
    expect(eyebrows[0].textContent).toContain('1 buyable');
    expect(eyebrows[0].textContent).not.toContain('for sale,');
  });

  it('offers Cancel on the own row and triggers the cancel flow; another seller\'s row still offers Buy', async () => {
    setup();
    fakeApi.marketData = [
      {
        id: '12',
        credits: 5_000,
        price: 500,
        token: 'mUSD1',
        maturesAt: 1756000000,
        matured: true,
        mine: false,
      },
      {
        id: '3',
        credits: 2_000,
        price: 200,
        token: 'mUSD1',
        maturesAt: 1755900000,
        matured: true,
        mine: true,
      },
    ];
    await store.loadMarket();
    fixture.detectChanges();

    const foreignBtn = buyButtonForRow(0);
    expect(foreignBtn).not.toBeNull();
    expect(foreignBtn!.textContent?.trim()).toBe('Buy');

    const ownRow = rows()[1];
    const ownButtons = Array.from(ownRow.querySelectorAll('button'));
    expect(ownButtons.map((b) => b.textContent?.trim())).toEqual(['Cancel']);

    const marketCallsBefore = fakeApi.marketCalls;
    (ownButtons[0] as HTMLButtonElement).click();
    await tick();

    // The cancel went through the sell flow with the row's on-chain id, and
    // the market list refreshed afterwards.
    expect(cancelSpy).toEqual(['3']);
    expect(store.listingsError()).toBeNull();
    expect(fakeApi.marketCalls).toBe(marketCallsBefore + 1);
    expect(buySpy.calls.length).toBe(0);
  });

  it('a confirmed buy lands on the overview and starts the balance confirmation', async () => {
    setup();
    fakeApi.marketData = [
      {
        id: '12',
        credits: 5_000,
        price: 500,
        token: 'mUSD1',
        maturesAt: 1756000000,
        matured: true,
        mine: false,
      },
    ];
    await store.loadMarket();
    fixture.detectChanges();

    const accountCallsBefore = fakeApi.marketCalls; // not used for balance
    const prevBalance = store.balance();
    buyButtonForRow(0)!.click();
    await tick();

    expect(buySpy.calls).toEqual([['12', 500, 'mUSD1', undefined]]);
    expect(navigateSpy.length).toBe(1);
    expect(navigateSpy[0]).toEqual(['/overview']);
    // The settlement returned only after the credits moved, so the overview
    // card is complete from the first frame and no local twin row is
    // invented for the market list to reconcile later.
    expect(store.buyCard()?.complete).toBe(true);
    expect(store.buyCard()?.credits).toBe(5_000);
    expect(store.listings().some((l) => l.role === 'buying')).toBe(false);
    // The bought credits arrive through the same balance path: the
    // announcement reloaded the account off the previous figure.
    expect(store.balance()).toBe(prevBalance);
    void accountCallsBefore;
  });

  it('a receipt failure discards the pending buy and shows the classified error in place', async () => {
    setup();
    fakeApi.marketData = [
      {
        id: '12',
        credits: 5_000,
        price: 500,
        token: 'mUSD1',
        maturesAt: 1756000000,
        matured: true,
        mine: false,
      },
    ];
    await store.loadMarket();
    fixture.detectChanges();

    // The buy fails inside the service: no announcement, no navigation, and
    // only the classified error remains on the market screen.
    buySpy.rejectNextReceipt = true;
    buyButtonForRow(0)!.click();
    await tick();

    expect(navigateSpy.length).toBe(0);
    expect(store.buyCard()).toBeNull();
    expect(store.listings().some((l) => l.role === 'buying')).toBe(false);
    expect(root().textContent).toContain('reverted on-chain');
  });

  it('a bought-by-another failure shows a plain notice and refreshes the list', async () => {
    setup();
    fakeApi.marketData = [
      {
        id: '12',
        credits: 5_000,
        price: 500,
        token: 'mUSD1',
        maturesAt: 1756000000,
        matured: true,
        mine: false,
      },
    ];
    await store.loadMarket();
    fixture.detectChanges();

    const marketCallsBefore = fakeApi.marketCalls;
    buySpy.rejectNext = true;
    buySpy.listingGone.set(true);
    buySpy.error.set('Someone else bought this listing first.');
    buyButtonForRow(0)!.click();
    await tick();

    // The list was refreshed again after the lost race.
    expect(fakeApi.marketCalls).toBe(marketCallsBefore + 1);
    // A plain muted notice, not an error block.
    expect(root().textContent).toContain('Nothing was paid');
    expect(root().querySelector('.state-block')).toBeNull();
    // No navigation away on a lost race.
    expect(navigateSpy.length).toBe(0);
  });

  it('scrolls the table in its own box, never the page', async () => {
    setup();
    fakeApi.marketData = [
      {
        id: '12',
        credits: 5_000,
        price: 500,
        token: 'mUSD1',
        maturesAt: 1756000000,
        matured: true,
        mine: false,
      },
    ];
    await store.loadMarket();
    fixture.detectChanges();

    const page = root().querySelector('section.page')!;
    expect(page).toBeTruthy();
    expect(page.classList).not.toContain('table-wrap');
    const table = page.querySelector('table.tbl')!;
    expect(table.closest('.table-wrap')).toBeTruthy();
    expect(table.parentElement!.classList).toContain('table-wrap');
  });

  it('a wallet rejection renders a dismissible notice, not a page error', async () => {
    setup();
    fakeApi.marketData = [
      { id: '12', credits: 5_000, price: 500, token: 'mUSD1', maturesAt: 1756000000, matured: true, mine: false },
      { id: '3', credits: 2_000, price: 200, token: 'mUSD1', maturesAt: 1755900000, matured: true, mine: true },
    ];
    await store.loadMarket();

    // The buy rejection.
    buySpy.error.set(BUY_ERRORS.rejected);
    fixture.detectChanges();
    expect(root().textContent).toContain(BUY_ERRORS.rejected);
    expect(root().textContent).not.toContain('Something went wrong');
    let notices = root().querySelectorAll('.notice-x');
    expect(notices.length).toBe(1);
    (notices[0].querySelector('.state-close') as HTMLButtonElement).click();
    fixture.detectChanges();
    expect(buySpy.error()).toBeNull();
    expect(root().querySelectorAll('.notice-x').length).toBe(0);

    // The cancel rejection, the live-test sequence: a second click while
    // MetaMask was confirming.
    sellFake.error.set(SELL_ERRORS.rejected);
    fixture.detectChanges();
    expect(root().textContent).toContain(SELL_ERRORS.rejected);
    notices = root().querySelectorAll('.notice-x');
    expect(notices.length).toBe(1);
    (notices[0].querySelector('.state-close') as HTMLButtonElement).click();
    fixture.detectChanges();
    expect(sellFake.error()).toBeNull();
    expect(root().querySelectorAll('.notice-x').length).toBe(0);
  });

  it('a non-rejection buy error keeps the error card with a working X', async () => {
    setup();
    buySpy.error.set(BUY_ERRORS.insufficient);
    fixture.detectChanges();

    expect(root().textContent).toContain(BUY_ERRORS.insufficient);
    const x = root().querySelector('.state-block .state-close') as HTMLButtonElement;
    expect(x).not.toBeNull();
    x.click();
    fixture.detectChanges();
    expect(buySpy.error()).toBeNull();
    expect(root().querySelector('.state-block')).toBeNull();
  });

  it('re-entering the screen clears both flows\' published errors (navigate away and back)', async () => {
    setup();
    await tick();

    buySpy.error.set(BUY_ERRORS.rejected);
    sellFake.error.set(SELL_ERRORS.rejected);
    fixture.detectChanges();

    // Leaving destroys the component; coming back constructs a fresh one,
    // which must clear both flows' errors on entry.
    fixture.destroy();
    const fresh = TestBed.createComponent(MarketComponent);
    fresh.detectChanges();
    expect(buySpy.error()).toBeNull();
    expect(sellFake.error()).toBeNull();
    fresh.destroy();
  });

  it('the Buy and Cancel buttons disable with a pending indicator while a prompt is open', async () => {
    setup();
    fakeApi.marketData = [
      { id: '12', credits: 5_000, price: 500, token: 'mUSD1', maturesAt: 1756000000, matured: true, mine: false },
      { id: '3', credits: 2_000, price: 200, token: 'mUSD1', maturesAt: 1755900000, matured: true, mine: true },
    ];
    await store.loadMarket();

    buySpy.step.set('buying');
    sellFake.step.set('cancelling');
    fixture.detectChanges();

    // The Buy button: disabled, pending label.
    const buyBtn = buyButtonForRow(0) as HTMLButtonElement;
    expect(buyBtn.disabled).toBe(true);
    expect(buyBtn.textContent?.trim()).toBe('Buying...');

    // The own-row Cancel button: disabled, pending label.
    const ownButtons = Array.from(rows()[1].querySelectorAll('button'));
    expect(ownButtons.map((b) => b.textContent?.trim())).toEqual(['Cancelling...']);
    expect((ownButtons[0] as HTMLButtonElement).disabled).toBe(true);

    // A second click cannot start a second prompt for the same action.
    const callsBefore = buySpy.calls.length;
    const cancelCallsBefore = cancelSpy.length;
    const comp = fixture.componentInstance as unknown as { doBuy(l: MarketListing): Promise<void>; doCancel(l: MarketListing): Promise<void> };
    await comp.doBuy(fakeApi.marketData[0]);
    await comp.doCancel(fakeApi.marketData[1]);
    expect(buySpy.calls.length).toBe(callsBefore);
    expect(cancelSpy.length).toBe(cancelCallsBefore);
  });

  it('offers the Sell credits action once, in the page header, and opens the sell flow in place', async () => {
    setup();
    fakeApi.marketData = [];
    await store.loadMarket();
    fixture.detectChanges();

    // One place carries the action; the empty card only states the situation.
    const headBtn = root().querySelector('.page-head-row .btn-primary') as HTMLButtonElement;
    expect(headBtn.textContent?.trim()).toBe('Sell credits');
    const emptyCard = root().querySelector('slf-empty-block')!.closest('.card')!;
    expect(emptyCard.querySelector('button')).toBeNull();
    expect(root().querySelector('slf-sell')).toBeNull();

    // The header action opens the panel: the existing flow is hosted, the
    // user stays on the market, and the empty state is still behind it.
    headBtn.click();
    fixture.detectChanges();
    expect(root().querySelector('slf-sell')).not.toBeNull();
    expect(navigateSpy.length).toBe(0);
    expect(root().querySelector('slf-empty-block')).not.toBeNull();
  });

  it('a successful listing refreshes the market list and shows the new row', async () => {
    setup();
    fakeApi.marketData = [
      { id: '12', credits: 5_000, price: 500, token: 'mUSD1', maturesAt: 1756000000, matured: true, mine: false },
    ];
    await store.loadMarket();
    fixture.detectChanges();
    const marketCallsBefore = fakeApi.marketCalls;

    (root().querySelector('.page-head-row .btn-primary') as HTMLButtonElement).click();
    fixture.detectChanges();

    // Drive the hosted flow through its real template.
    const credits = root().querySelector('#sellCredits') as HTMLInputElement;
    credits.value = '1000';
    credits.dispatchEvent(new Event('input'));
    const price = root().querySelector('#sellPrice') as HTMLInputElement;
    price.value = '5';
    price.dispatchEvent(new Event('input'));
    fixture.detectChanges();

    const listBtn = root().querySelector('slf-sell .btn-primary') as HTMLButtonElement;
    expect(listBtn.disabled).toBe(false);
    listBtn.click();
    // The server mirror records the listing as soon as the announcement
    // lands; the refresh the announcement triggers reads it.
    fakeApi.marketData.push({
      id: 'own-1', credits: 1_000, price: 500, token: 'mUSD1', maturesAt: 1756000000, matured: false, mine: true,
    });
    await tick();
    fixture.detectChanges();
    TestBed.flushEffects();
    await tick();
    fixture.detectChanges();

    // The announcement triggered exactly one market refresh...
    expect(fakeApi.marketCalls).toBe(marketCallsBefore + 1);
    // ...and the new row is on screen, marked as the customer's own. Rows
    // inside the hosted flow's own My-listings table are excluded.
    const marketRows = Array.from(root().querySelectorAll('tbody tr'))
      .filter((tr) => tr.closest('.sell-panel') === null);
    expect(marketRows.length).toBe(2);
    expect(marketRows[1].textContent).toContain('Yours');
    // The success path cleared the hosted form.
    expect((root().querySelector('#sellCredits') as HTMLInputElement).value).toBe('');
  });

  it('closing the panel leaves the market untouched and no sell state behind', async () => {
    setup();
    fakeApi.marketData = [
      { id: '12', credits: 5_000, price: 500, token: 'mUSD1', maturesAt: 1756000000, matured: true, mine: false },
    ];
    await store.loadMarket();
    fixture.detectChanges();
    const marketCallsBefore = fakeApi.marketCalls;
    const rowsBefore = rows().length;

    // Open, leave form state behind, and close again.
    (root().querySelector('.page-head-row .btn-primary') as HTMLButtonElement).click();
    fixture.detectChanges();
    const credits = root().querySelector('#sellCredits') as HTMLInputElement;
    credits.value = '42';
    credits.dispatchEvent(new Event('input'));
    fixture.detectChanges();

    (root().querySelector('.sell-panel-bar button') as HTMLButtonElement).click();
    fixture.detectChanges();

    // The panel is gone with its state; the market was never read again.
    expect(root().querySelector('slf-sell')).toBeNull();
    expect(rows().length).toBe(rowsBefore);
    expect(fakeApi.marketCalls).toBe(marketCallsBefore);
    expect(store.pendingListingId()).toBeNull();
    expect(sellFake.step()).toBe('idle');

    // Reopening gives a fresh form: nothing survived the close.
    (root().querySelector('.page-head-row .btn-primary') as HTMLButtonElement).click();
    fixture.detectChanges();
    expect((root().querySelector('#sellCredits') as HTMLInputElement).value).toBe('');
  });
});
