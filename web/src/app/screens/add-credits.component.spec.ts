import { computed, signal } from '@angular/core';
import { Component } from '@angular/core';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { provideRouter, Router } from '@angular/router';
import { vi } from 'vitest';
import { ConsoleStore } from '../core/console-store';
import { BreakpointService } from '../core/breakpoint';
import { SellService } from '../core/sell';
import { BuyService } from '../core/buy';
import { DepositService, DEPOSIT_ERRORS } from '../core/deposit';
import { ToastService } from '../core/toast';
import {
  Account, ApiError, AttestChain, ChainInfo,
  CreatedKey, DepositTarget, FundingStatus, Key,
  ListingPage, MarketConfirmBody, MarketPrepareBody, ModelList,
  Nonce, PreparedMarketTx, SLOTFARE_API, SlotfareApi,
  UsagePage, UsageProof, UsageQuery, UsageRowT,
  VerifyResult,
} from '../core/api';
import { Listing, MarketListing, Model, Wallet } from '../core/models';
import { environment } from '../../environments/environment';
import { ConsoleSession, WalletService } from '../core/wallet';
import { OPENING_BALANCE, OPENING_LOCKED } from '../core/mock-data';
import { AddCreditsComponent } from './add-credits.component';

const tick = (): Promise<void> => new Promise((r) => setTimeout(r));

const TARGET: DepositTarget = {
  rpc: 'https://api.devnet.solana.com',
  mint: 'DemoUSDCmint11111111111111111111111111111111',
  treasury: 'DemoTreasuryTok1111111111111111111111111111111',
  decimals: 6,
  cluster: 'devnet',
  explorer: 'https://explorer.solana.com',
  creditsPerUsd: 1000,
  microCreditsPerUsdBase: 10,
};

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

/** Enough of the console API for this screen and the store's session loads. */
class FakeApi implements SlotfareApi {
  /** When false, depositTarget() fails and the screen shows the
   *  not-configured state. */
  hasTarget = true;

  async nonce(_address: string): Promise<Nonce> {
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
  async createKey(_label: string): Promise<CreatedKey> {
    throw new ApiError(500, 'internal', 'no');
  }
  async revokeKey(_id: string): Promise<void> {}
  async usage(_q?: UsageQuery): Promise<UsagePage> {
    return { rows: [], nextCursor: null };
  }
  async funding(_id: string): Promise<FundingStatus> {
    throw new ApiError(404, 'not_found', 'unknown');
  }
  async fundingList(): Promise<FundingStatus[]> {
    return [];
  }
  async listings(_b?: string): Promise<ListingPage> {
    return { rows: [], nextCursor: null };
  }
  async market(): Promise<MarketListing[]> {
    return [];
  }
  async chains(): Promise<ChainInfo[]> {
    return [
      {
        chainKey: 0,
        name: 'Solana',
        chainId: 0,
        deposit: TARGET.treasury,
        explorer: TARGET.explorer,
        rpc: TARGET.rpc,
      },
    ];
  }
  async attestation(): Promise<AttestChain[]> {
    return [];
  }
  async models(): Promise<ModelList> {
    return { models: [], baseUrl: '' };
  }
  async usageProof(_id: string): Promise<UsageProof> {
    throw new ApiError(404, 'not_found', 'pending');
  }
  async depositTarget(): Promise<DepositTarget> {
    if (!this.hasTarget) throw new ApiError(501, 'not_implemented', '');
    return TARGET;
  }
  async createListing(_body: { credits: number; price: number }): Promise<Listing> {
    throw new Error('not implemented');
  }
  async cancelListing(_id: string): Promise<Listing> {
    throw new Error('not implemented');
  }
  async payListing(_id: string, _sig: string): Promise<Listing> {
    throw new Error('not implemented');
  }
  async prepareMarket(_body: MarketPrepareBody): Promise<PreparedMarketTx> {
    throw new Error('not implemented');
  }
  async confirmMarket(_body: MarketConfirmBody): Promise<Listing> {
    throw new Error('not implemented');
  }
}

/** A controllable DepositService stand-in: the screen reads its four
 *  signals and calls these three methods, nothing else. */
function makeDepositSpy(overrides: { hasTarget?: boolean; rejectWith?: string } = {}) {
  const calls = { deposit: [] as number[], loadTarget: 0, dismissError: 0 };
  const busy = signal(false);
  const error = signal<string | null>(null);
  const target = signal<DepositTarget | null>(null);
  const spy = {
    busy,
    error,
    target,
    calls,
    nextSignature: 'Sig111Deposit222Deposit333',
    loadTarget: async () => {
      calls.loadTarget += 1;
      if (overrides.hasTarget !== false) target.set({ ...TARGET });
    },
    dismissError: () => {
      calls.dismissError += 1;
      error.set(null);
    },
    deposit: async (amountUsd: number): Promise<string> => {
      calls.deposit.push(amountUsd);
      if (overrides.rejectWith !== undefined) {
        error.set(overrides.rejectWith);
        throw new Error(overrides.rejectWith);
      }
      return spy.nextSignature;
    },
  };
  return spy;
}

class DummyComponent {}

describe('AddCreditsComponent', () => {
  // The store's stream gate is environment.useMock; keep it true so
  // these specs never open a real EventSource (jsdom has none).
  beforeAll(() => {
    environment.useMock = true;
  });

  let fixture: ComponentFixture<AddCreditsComponent>;
  let fakeApi: FakeApi;
  let fakeWallet: WalletService;
  let store: ConsoleStore;
  let depositSpy: ReturnType<typeof makeDepositSpy>;
  let toast: ToastService;
  let toastSay: ReturnType<typeof vi.spyOn>;
  let router: Router;
  let navigateSpy: ReturnType<typeof vi.spyOn>;
  let breakpoints: BreakpointService;

  function setup(depositOpts: Parameters<typeof makeDepositSpy>[0] = {}) {
    fakeApi = new FakeApi();
    fakeWallet = makeFakeWallet();
    depositSpy = makeDepositSpy(depositOpts);
    TestBed.configureTestingModule({
      imports: [AddCreditsComponent],
      providers: [
        { provide: SLOTFARE_API, useValue: fakeApi },
        { provide: WalletService, useValue: fakeWallet },
        { provide: SellService, useValue: {
          step: signal('idle'),
          error: signal<string | null>(null),
          busy: computed(() => false),
          list: async (): Promise<Listing> => {
            throw new Error('not implemented');
          },
          cancel: async (): Promise<void> => {},
          dismissError: () => undefined,
        } },
        { provide: BuyService, useValue: {
          step: signal('idle'),
          error: signal<string | null>(null),
          listingGone: signal(false),
          busy: computed(() => false),
          buy: async (): Promise<string> => '0xbuytx',
          dismissError: () => undefined,
        } },
        { provide: DepositService, useValue: depositSpy },
        ConsoleStore,
        ToastService,
        provideRouter([
          { path: 'funding/:id', component: DummyComponent },
          { path: 'sell', component: DummyComponent },
        ]),
      ],
    });
    fixture = TestBed.createComponent(AddCreditsComponent);
    store = TestBed.inject(ConsoleStore);
    toast = TestBed.inject(ToastService);
    toastSay = vi.spyOn(toast, 'say');
    router = TestBed.inject(Router);
    navigateSpy = vi.spyOn(router, 'navigate').mockResolvedValue(true);
    breakpoints = TestBed.inject(BreakpointService);
    fixture.detectChanges();
    // Establish a session so the store loads the account balance.
    fakeWallet.address.set('0xabc');
    fakeWallet.accountId.set('0xmockacct');
    fakeWallet.token.set('mock-token');
    TestBed.flushEffects();
  }

  afterEach(() => {
    store.stopPolling();
    fixture.destroy();
    TestBed.resetTestingModule();
  });

  function root(): HTMLElement {
    return fixture.nativeElement as unknown as HTMLElement;
  }

  function payButton(): HTMLButtonElement {
    return root().querySelector('button.btn-primary') as HTMLButtonElement;
  }

  function kv(label: string): string {
    const rows = Array.from(root().querySelectorAll('.kv'));
    const row = rows.find((r) => r.querySelector('span')?.textContent?.trim() === label);
    return row?.querySelector('b')?.textContent?.trim() ?? '';
  }

  it('renders the presets, the fixed rate and the default review figures', async () => {
    setup();
    await tick();
    fixture.detectChanges();

    const presets = root().querySelectorAll('.choice-grid[aria-label="Preset amounts"] button');
    expect(presets.length).toBe(4);
    expect(presets[0].textContent).toContain('$25');
    expect(presets[3].textContent).toContain('$500');

    expect(kv('You pay')).toBe('250.00 USDC');
    expect(kv('Rate')).toBe('1,000 credits / $1.00');
    expect(kv('Credits minted')).toContain('250,000');
    expect(kv('New balance')).toContain('662,860');
    expect(payButton().textContent).toContain('Pay 250.00 USDC');
    expect(payButton().disabled).toBe(false);
  });

  it('a preset click re-prices the review in the same frame', async () => {
    setup();
    await tick();
    fixture.detectChanges();

    const presets = Array.from(
      root().querySelectorAll<HTMLButtonElement>('.choice-grid[aria-label="Preset amounts"] button'),
    );
    presets[0].click();
    fixture.detectChanges();

    expect(kv('You pay')).toBe('25.00 USDC');
    expect(kv('Credits minted')).toContain('25,000');
    expect(payButton().textContent).toContain('Pay 25.00 USDC');
    expect(presets[0].getAttribute('aria-pressed')).toBe('true');
  });

  it('an amount outside the bounds disables Pay with an honest reason', async () => {
    setup();
    await tick();
    fixture.detectChanges();

    const input = root().querySelector('#customAmount') as HTMLInputElement;
    input.value = '4';
    input.dispatchEvent(new Event('input'));
    fixture.detectChanges();

    expect(payButton().disabled).toBe(true);
    expect(root().textContent).toContain('Enter an amount between $5 and $50,000.');
  });

  it('renders the deposit target facts and the treasury explorer link', async () => {
    setup();
    await tick();
    fixture.detectChanges();

    const html = root().textContent ?? '';
    expect(html).toContain('devnet');
    expect(html).toContain('Demo...1111');
    const link = root().querySelector('.kv a[target="_blank"]') as HTMLAnchorElement;
    expect(link.getAttribute('href')).toBe(
      'https://explorer.solana.com/address/DemoTreasuryTok1111111111111111111111111111111',
    );
  });

  it('without a configured deposit target Pay stays disabled with the not-configured reason', async () => {
    setup({ hasTarget: false });
    await tick();
    fixture.detectChanges();

    expect(depositSpy.target()).toBeNull();
    expect(payButton().disabled).toBe(true);
    expect(root().textContent).toContain(DEPOSIT_ERRORS.noTarget);
    expect(root().textContent).toContain('Deposits not configured');
  });

  it('pay() sends the amount, announces the purchase, toasts and opens the trace', async () => {
    setup();
    await tick();
    fixture.detectChanges();

    payButton().click();
    await tick();
    fixture.detectChanges();

    expect(depositSpy.calls.deposit).toEqual([250]);
    expect(store.fundingHistory().map((p) => p.purchaseId)).toEqual([depositSpy.nextSignature]);
    expect(store.fundingStatus()?.amountUsd).toBe(250);
    expect(store.fundingStatus()?.token).toBe('USDC');
    expect(toastSay).toHaveBeenCalledWith(
      expect.stringContaining('Deposit sent:'),
    );
    expect(navigateSpy).toHaveBeenCalledWith(['/funding', depositSpy.nextSignature]);
  });

  it('a rejection renders the dismissible notice with a working X', async () => {
    setup({ rejectWith: DEPOSIT_ERRORS.rejected });
    await tick();
    fixture.detectChanges();

    payButton().click();
    await tick();
    fixture.detectChanges();

    const notice = root().querySelector('.notice-x') as HTMLElement;
    expect(notice.textContent).toContain(DEPOSIT_ERRORS.rejected);
    expect(root().querySelector('slf-error-block')).toBeNull();
    expect(navigateSpy).not.toHaveBeenCalled();

    (notice.querySelector('.state-close') as HTMLButtonElement).click();
    fixture.detectChanges();
    expect(root().querySelector('.notice-x')).toBeNull();
    expect(depositSpy.error()).toBeNull();
  });

  it('a non-rejection failure keeps the error card with a working X', async () => {
    setup({ rejectWith: 'transfer failed: something upstream said no' });
    await tick();
    fixture.detectChanges();

    payButton().click();
    await tick();
    fixture.detectChanges();

    expect(root().textContent).toContain('transfer failed: something upstream said no');
    expect(root().querySelector('.notice-x')).toBeNull();

    const dismiss = root().querySelector('slf-error-block .state-close') as HTMLButtonElement;
    dismiss.click();
    fixture.detectChanges();
    expect(root().textContent).not.toContain('transfer failed: something upstream said no');
  });

  it('the button disables with a wallet-prompt label while a payment is in flight', async () => {
    setup();
    await tick();
    fixture.detectChanges();

    depositSpy.busy.set(true);
    fixture.detectChanges();

    expect(payButton().disabled).toBe(true);
    expect(payButton().textContent).toContain('Confirm in your wallet...');
  });

  it('re-entering the screen clears a published error', async () => {
    setup({ rejectWith: DEPOSIT_ERRORS.rejected });
    await tick();
    fixture.detectChanges();

    payButton().click();
    await tick();
    fixture.detectChanges();
    expect(root().querySelector('.notice-x')).not.toBeNull();

    // Navigate away and back: a fresh screen starts clean.
    fixture.destroy();
    fixture = TestBed.createComponent(AddCreditsComponent);
    fixture.detectChanges();
    await tick();
    fixture.detectChanges();

    expect(root().querySelector('.notice-x')).toBeNull();
    expect(depositSpy.error()).toBeNull();
  });

  it('collapses the card split to one column on mobile, two on tablet and desktop', async () => {
    setup();
    await tick();
    fixture.detectChanges();

    breakpoints.setForTest('mobile', true);
    fixture.detectChanges();
    expect(root().querySelector('.split')?.classList.contains('cols-1')).toBe(true);

    breakpoints.setForTest('tablet', false);
    fixture.detectChanges();
    expect(root().querySelector('.split')?.classList.contains('cols-1')).toBe(false);

    breakpoints.setForTest('desktop', false);
    fixture.detectChanges();
    expect(root().querySelector('.split')?.classList.contains('cols-1')).toBe(false);
  });

  it('keeps the payment choices and the amount in labeled controls', async () => {
    setup();
    await tick();
    fixture.detectChanges();

    const tokenGroup = root().querySelector('.choice-grid[aria-label="Payment token"]');
    expect(tokenGroup).not.toBeNull();
    expect(tokenGroup?.querySelector('button.choice')?.getAttribute('aria-pressed')).toBe('true');
    expect(root().textContent).toContain('USDC');

    const input = root().querySelector('#customAmount') as HTMLInputElement;
    const label = root().querySelector('label[for="customAmount"]');
    expect(label).not.toBeNull();
    expect(input.value).toBe('250');
  });
});
