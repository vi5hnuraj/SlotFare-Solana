import { computed, signal } from '@angular/core';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { Router } from '@angular/router';
import { ConsoleStore } from '../core/console-store';
import { BreakpointService } from '../core/breakpoint';
import { SellService, SELL_ERRORS } from '../core/sell';
import { ToastService } from '../core/toast';
import {
  Account, ApiError, AttestChain, ChainInfo,
  CreatedKey, DepositTarget, FundingStatus, Key,
  ListingPage, MarketConfirmBody, MarketPrepareBody, ModelList,
  Nonce, PreparedMarketTx, SLOTFARE_API, SlotfareApi,
  UsagePage, UsageProof, UsageQuery, VerifyResult,
} from '../core/api';
import { Listing, MarketListing, Model } from '../core/models';
import { environment } from '../../environments/environment';
import { ConsoleSession, WalletService } from '../core/wallet';
import { Wallet } from '../core/models';
import { OPENING_BALANCE, OPENING_LOCKED } from '../core/mock-data';
import { num } from '../core/pricing';
import { SellComponent } from './sell.component';

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
  listingsData: Listing[] = [];
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
      balanceCredits: OPENING_BALANCE,
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

describe('SellComponent', () => {
  // The store's stream gate is environment.useMock; keep it true so
  // these specs never open a real EventSource (jsdom has none).
  beforeAll(() => {
    environment.useMock = true;
  });
  let fixture: ComponentFixture<SellComponent>;
  let fakeApi: FakeApi;
  let fakeWallet: WalletService;
  let store: ConsoleStore;
  let sellSpy: { listCalls: number[]; cancelCalls: string[] };
  /** The SellService stand-in's signals, so tests can drive rejection and
   *  pending states. */
  let sellFake: {
    step: ReturnType<typeof signal<string>>;
    error: ReturnType<typeof signal<string | null>>;
  };

  function setup() {
    fakeApi = new FakeApi();
    fakeWallet = makeFakeWallet();
    sellSpy = { listCalls: [], cancelCalls: [] };
    const step = signal('idle');
    const error = signal<string | null>(null);
    sellFake = { step, error };
    TestBed.configureTestingModule({
      imports: [SellComponent],
      providers: [
        { provide: SLOTFARE_API, useValue: fakeApi },
        { provide: WalletService, useValue: fakeWallet },
        { provide: SellService, useValue: {
          step,
          error,
          txHash: signal<string | null>(null),
          busy: computed(() => step() !== 'idle' && step() !== 'done'),
          list: async (credits: number, symbol: string, priceUsd: number): Promise<Listing> => {
            sellSpy.listCalls.push(credits, priceUsd);
            return {
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
            };
          },
          cancel: async (id: string): Promise<string> => {
            sellSpy.cancelCalls.push(id);
            return '0xcanceltx000000000000000000000000000000000000000000000000bb';
          },
          dismissError: () => {
            error.set(null);
            step.set('idle');
          },
        } },
        ConsoleStore,
        ToastService,
        { provide: Router, useValue: { navigate: () => Promise.resolve(true) } },
      ],
    });
    fixture = TestBed.createComponent(SellComponent);
    store = TestBed.inject(ConsoleStore);
    fixture.detectChanges();
    // Establish a session so the store loads the account and the listings.
    fakeWallet.address.set('0xabc');
    fakeWallet.accountId.set('0xmockacct');
    fakeWallet.token.set('mock-token');
    TestBed.flushEffects();
  }

  afterEach(() => {
    store.stopPolling();
  });

  function setForm(credits: string, price: string): void {
    const c = fixture.nativeElement.querySelector('#sellCredits') as HTMLInputElement;
    c.value = credits;
    c.dispatchEvent(new Event('input'));
    const p = fixture.nativeElement.querySelector('#sellPrice') as HTMLInputElement;
    p.value = price;
    p.dispatchEvent(new Event('input'));
    fixture.detectChanges();
  }

  function root(): HTMLElement {
    return fixture.nativeElement as unknown as HTMLElement;
  }

  function listButton(): HTMLButtonElement {
    const buttons = Array.from(root().querySelectorAll('button'));
    const b = buttons.find((x) => x.textContent?.includes('List'));
    expect(b).toBeDefined();
    return b as HTMLButtonElement;
  }

  it('refuses over-spendable, zero credits and zero price without touching the wallet', async () => {
    setup();
    await tick();

    // Zero credits.
    setForm('0', '100');
    expect(listButton().disabled).toBe(true);
    setForm('', '100');
    expect(listButton().disabled).toBe(true);

    // Zero price.
    setForm('100', '0');
    expect(listButton().disabled).toBe(true);
    setForm('100', '');
    expect(listButton().disabled).toBe(true);

    // More than spendable.
    setForm(String(OPENING_BALANCE + 1), '100');
    expect(listButton().disabled).toBe(true);

    // A valid amount enables it.
    setForm('100', '100');
    expect(listButton().disabled).toBe(false);

    // The wallet was never opened for any of the refusals.
    expect(sellSpy.listCalls.length).toBe(0);
  });

  it('a sent listing appears open immediately and is replaced by the server row', async () => {
    setup();
    await tick();
    expect(store.listings().length).toBe(0);

    setForm('5000', '500');
    listButton().click();
    await tick();

    // The D57 local row is visible the moment the wallet returns.
    expect(sellSpy.listCalls.length).toBeGreaterThan(0);
    expect(store.listings().map((l) => l.id)).toEqual(['0xlistingtx000000000000000000000000000000000000000000000000aa']);
    expect(store.listings()[0].state).toBe('open');
    expect(store.openListingsCount()).toBe(1);
    fixture.detectChanges();
    expect(fixture.nativeElement.outerHTML).toContain(num(5_000));

    // The poll reports it: same listedTx, the server's numeric listing id.
    // The server row replaces the local twin.
    fakeApi.listingsData = [
      { id: '7', listedTx: '0xlistingtx000000000000000000000000000000000000000000000000aa', state: 'open', role: 'selling', credits: 5_000, price: 500, token: 'mUSD1', matured: false, createdAt: '2026-08-22T20:00:00Z' },
    ];
    await store.loadListings();
    expect(store.listings().length).toBe(1);
    expect(store.listings()[0].id).toBe('7');
    expect(store.listings()[0].createdAt).toBe('2026-08-22T20:00:00Z');
  });

  it('cancel sends for the right listing id and the row follows the server', async () => {
    setup();
    fakeApi.listingsData = [
      { id: '7', listedTx: '0xlist7', state: 'open', role: 'selling', credits: 5_000, price: 500, token: 'mUSD1', matured: false, createdAt: '2026-08-22T20:00:00Z' },
    ];
    await store.loadListings();
    fixture.detectChanges();

    const cancelButtons = Array.from(root().querySelectorAll('button'))
      .filter((b) => b.textContent?.trim() === 'Cancel');
    expect(cancelButtons.length).toBe(1);
    cancelButtons[0].click();
    await tick();

    expect(sellSpy.cancelCalls).toEqual(['7']);
    // The server still reports open: the row stays open, never flipped by the click.
    expect(store.listings()[0].state).toBe('open');

    // The server moves it to cancelled; the refresh follows.
    fakeApi.listingsData = [
      { id: '7', listedTx: '0xlist7', state: 'cancelled', role: 'selling', credits: 5_000, price: 500, token: 'mUSD1', matured: false, createdAt: '2026-08-22T20:00:00Z' },
    ];
    await store.loadListings();
    fixture.detectChanges();
    expect(store.listings()[0].state).toBe('cancelled');
    const after = Array.from(root().querySelectorAll('button'))
      .filter((b) => b.textContent?.trim() === 'Cancel');
    expect(after.length).toBe(0);
  });

  it('renders listing prices scaled by the pay token decimals', async () => {
    setup();
    const mUSD1 = { address: 'DemoUSDCmint11111111111111111111111111111111' };
    fakeApi.listingsData = [
      { id: '7', listedTx: '0xlist7', state: 'open', role: 'selling', credits: 5_000, price: 5_000_000, token: 'mUSD1', tokenAddress: mUSD1.address, tokenDecimals: 6, matured: true, createdAt: '2026-08-22T20:00:00Z' },
    ];
    await store.loadListings();
    fixture.detectChanges();

    // The raw wire amount is scaled to its human figure with the symbol;
    // the unscaled raw figure never reaches the cell.
    const html = fixture.nativeElement.outerHTML;
    expect(html).toContain('5.00');
    expect(html).toContain('mUSD1');
    expect(html).not.toContain('5,000,000.00');
  });

  it('a bought listing never appears in My listings; a sold sell row still reads Sold', async () => {
    setup();
    fakeApi.listingsData = [
      { id: '7', listedTx: '0xlist7', state: 'open', role: 'selling', credits: 5_000, price: 500, token: 'mUSD1', matured: false, createdAt: '2026-08-22T20:00:00Z' },
      { id: '9', listedTx: '0xlist9', state: 'sold', role: 'selling', credits: 3_000, price: 300, token: 'mUSD1', matured: true, createdAt: '2026-08-21T00:00:00Z' },
      { id: 'b1', listedTx: '0xbuytx1', state: 'sold', role: 'buying', credits: 7_777, price: 777, token: 'mUSD1', matured: true, createdAt: '2026-08-23T00:00:00Z' },
    ];
    await store.loadListings();
    fixture.detectChanges();

    // The card is fed by the sell-side rows only: two rows, no buying row.
    expect(store.sellListings().map((l) => l.id)).toEqual(['7', '9']);
    const body = root().querySelector('tbody');
    expect(body).not.toBeNull();
    const trs = Array.from(body!.querySelectorAll('tr'));
    expect(trs.length).toBe(2);

    // The bought listing's data never reaches the card.
    const html = fixture.nativeElement.outerHTML;
    expect(html).not.toContain('b1');
    expect(html).not.toContain(num(7_777));

    // A sold sell row still renders its Sold pill on the Sell screen.
    const soldRow = trs.find((tr) => tr.querySelector('.pill-value') !== null);
    expect(soldRow).toBeDefined();
    expect(soldRow!.textContent?.includes('sold')).toBe(true);
    // The open sell row keeps its open pill and offers a Cancel.
    const openRow = trs.find((tr) => tr.querySelector('.pill-ok') !== null);
    expect(openRow).toBeDefined();
  });

  it('with only a bought listing, My listings shows the empty state', async () => {
    setup();
    fakeApi.listingsData = [
      { id: 'b1', listedTx: '0xbuytx1', state: 'sold', role: 'buying', credits: 5_000, price: 500, token: 'mUSD1', matured: true, createdAt: '2026-08-23T00:00:00Z' },
    ];
    await store.loadListings();
    fixture.detectChanges();

    // No sell-side rows: the card falls back to the "No listings yet" block.
    expect(store.sellListings().length).toBe(0);
    expect(fixture.nativeElement.outerHTML).toContain('No listings yet');
    expect(root().querySelector('tbody')).toBeNull();
  });

  it('renders spendable and locked as distinct figures from one balance payload', async () => {
    setup();
    await tick();
    fixture.detectChanges();

    // One payload with both figures drives two distinct rendered numbers.
    expect(store.balance()).toBe(OPENING_BALANCE);
    expect(store.locked()).toBe(OPENING_LOCKED);
    const html = fixture.nativeElement.outerHTML;
    expect(html).toContain(num(OPENING_BALANCE));
    expect(html).toContain(num(OPENING_LOCKED));
    expect(html).toContain('locked');
    expect(num(OPENING_BALANCE)).not.toBe(num(OPENING_LOCKED));
  });

  it('scrolls the table in its own box, never the page', async () => {
    setup();
    fakeApi.listingsData = [
      { id: '7', listedTx: '0xlist7', state: 'open', role: 'selling', credits: 5_000, price: 500, token: 'mUSD1', matured: false, createdAt: '2026-08-22T20:00:00Z' },
    ];
    await store.loadListings();
    fixture.detectChanges();

    const page = root().querySelector('section.page')!;
    expect(page).toBeTruthy();
    expect(page.classList).not.toContain('table-wrap');
    const table = page.querySelector('table.tbl')!;
    expect(table.closest('.table-wrap')).toBeTruthy();
    expect(table.parentElement!.classList).toContain('table-wrap');
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
  });

  it('a wallet rejection renders a dismissible notice, not a page error', async () => {
    setup();
    await tick();

    sellFake.error.set(SELL_ERRORS.rejected);
    fixture.detectChanges();

    // The rejection is an inline notice on the flow that asked, never the
    // page-level "Something went wrong" card.
    const html = root().textContent ?? '';
    expect(html).toContain(SELL_ERRORS.rejected);
    expect(html).not.toContain('Something went wrong');
    expect(root().querySelector('.notice-x')).not.toBeNull();

    // The X clears the signal and the card leaves.
    const x = root().querySelector('.notice-x .state-close') as HTMLButtonElement;
    x.click();
    fixture.detectChanges();
    expect(sellFake.error()).toBeNull();
    expect(root().querySelector('.notice-x')).toBeNull();
    expect(root().textContent).not.toContain(SELL_ERRORS.rejected);
  });

  it('a non-rejection sell error keeps the error card with a working X', async () => {
    setup();
    await tick();

    sellFake.error.set(SELL_ERRORS.insufficient);
    fixture.detectChanges();

    expect(root().textContent).toContain(SELL_ERRORS.insufficient);
    const card = root().querySelector('slf-error-block');
    expect(card).not.toBeNull();
    expect(root().querySelector('.notice-x')).toBeNull();

    const x = card!.querySelector('.state-close') as HTMLButtonElement;
    x.click();
    fixture.detectChanges();
    expect(sellFake.error()).toBeNull();
    expect(root().querySelector('slf-error-block')).toBeNull();
  });

  it('re-entering the screen clears a published rejection (navigate away and back)', async () => {
    setup();
    await tick();

    sellFake.error.set(SELL_ERRORS.rejected);
    fixture.detectChanges();
    expect(root().textContent).toContain(SELL_ERRORS.rejected);

    // Leaving destroys the component; coming back constructs a fresh one,
    // which must clear the flow's error on entry.
    fixture.destroy();
    const fresh = TestBed.createComponent(SellComponent);
    fresh.detectChanges();
    expect(sellFake.error()).toBeNull();
    expect(fresh.nativeElement.textContent).not.toContain(SELL_ERRORS.rejected);
    fresh.destroy();
  });

  it('a listings rejection renders an inline notice, not a page error', async () => {
    setup();
    fakeApi.listingsData = [];
    await tick();
    store.listingsError.set(SELL_ERRORS.rejected);
    fixture.detectChanges();

    // The rejection is an inline notice on the flow that asked, never the
    // page-level "Something went wrong" card.
    const html = root().textContent ?? '';
    expect(html).toContain(SELL_ERRORS.rejected);
    expect(html).not.toContain('Something went wrong');
    expect(root().querySelector('.notice-x')).not.toBeNull();
    expect(root().querySelector('slf-error-block')).toBeNull();

    // The X clears the signal and the notice leaves.
    const x = root().querySelector('.notice-x .state-close') as HTMLButtonElement;
    x.click();
    fixture.detectChanges();
    expect(store.listingsError()).toBeNull();
    expect(root().querySelector('.notice-x')).toBeNull();
  });

  it('dismissing a non-rejection listings error card clears the store signal', async () => {
    setup();
    fakeApi.listingsData = [];
    await tick();
    store.listingsError.set('Something went wrong');
    fixture.detectChanges();

    const card = root().querySelector('slf-error-block');
    expect(card).not.toBeNull();
    const x = card!.querySelector('.state-close') as HTMLButtonElement;
    expect(x).not.toBeNull();
    x.click();
    fixture.detectChanges();
    expect(store.listingsError()).toBeNull();
    expect(root().querySelector('slf-error-block')).toBeNull();
  });

  it('the initiating buttons disable with a pending indicator while a prompt is open', async () => {
    setup();
    await tick();
    setForm('100', '100');
    sellFake.step.set('cancelling');
    fixture.detectChanges();

    // The List button: disabled, pending label.
    const listBtn = Array.from(root().querySelectorAll('button'))
      .find((b) => b.textContent?.includes('Creating listing...')) as HTMLButtonElement;
    expect(listBtn).toBeDefined();
    expect(listBtn.disabled).toBe(true);

    // The row Cancel button: disabled, pending label.
    fakeApi.listingsData = [
      { id: '7', listedTx: '0xlist7', state: 'open', role: 'selling', credits: 5_000, price: 500, token: 'mUSD1', matured: false, createdAt: '2026-08-22T20:00:00Z' },
    ];
    await store.loadListings();
    fixture.detectChanges();
    const cancelButtons = Array.from(root().querySelectorAll('button'))
      .filter((b) => b.textContent?.trim() === 'Cancelling...');
    expect(cancelButtons.length).toBe(1);
    expect((cancelButtons[0] as HTMLButtonElement).disabled).toBe(true);

    // A second click cannot start a second prompt for the same action.
    const listCallsBefore = sellSpy.listCalls.length;
    const cancelCallsBefore = sellSpy.cancelCalls.length;
    const comp = fixture.componentInstance as unknown as { doList(): Promise<void>; cancel(l: { id: string }): Promise<void> };
    await comp.doList();
    await comp.cancel({ id: '7' });
    expect(sellSpy.listCalls.length).toBe(listCallsBefore);
    expect(sellSpy.cancelCalls.length).toBe(cancelCallsBefore);
  });
});
