import { vi } from 'vitest';
import { computed, signal } from '@angular/core';
import { Component } from '@angular/core';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { provideRouter, Router } from '@angular/router';
import { ConsoleStore } from '../core/console-store';
import { BreakpointService } from '../core/breakpoint';
import {
  Account, ApiError, AttestChain, ChainInfo,
  CreatedKey, DepositTarget, FundingStatus, Key,
  ListingPage, MarketConfirmBody, MarketPrepareBody, ModelList,
  Nonce, PreparedMarketTx, SLOTFARE_API, SlotfareApi,
  UsagePage, UsageProof, UsageQuery, VerifyResult,
} from '../core/api';
import { ConsoleSession, WalletService } from '../core/wallet';
import { Listing, MarketListing, Model, Wallet } from '../core/models';
import { environment } from '../../environments/environment';
import { ShellComponent } from './shell.component';

/** Let an effect-driven navigation settle. */
const tick = (): Promise<void> => new Promise((r) => setTimeout(r));

@Component({ selector: 'slf-trade-dummy', template: '' })
class TradeDummyComponent {}

/** The known address the fake wallet reports; the chip must render it
 *  truncated, with the full value in the title. */
const CONNECTED_ADDRESS = '0x5f4e3d2c1b0a9876543210fedcba0987654321ab';
const SHORTENED_ADDRESS = '0x5f4e...21ab';

function makeFakeWallet(): WalletService {
  const address = signal<string | null>(CONNECTED_ADDRESS);
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
  return {
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
    provider: null,
    connect: () => undefined,
    startSession: () => undefined,
    dismissError: () => {
      error.set(null);
    },
    clearSession: () => {
      token.set(null);
      accountId.set(null);
      error.set(null);
      restoredRdns.set(null);
      restoredWallet.set(null);
    },
  } as unknown as WalletService;
}

/** No session in these specs, so nothing calls the API; every method is a
 *  trivial or throwing stand-in. */
class EmptyApi implements SlotfareApi {
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
    return {
      accountId: '0xmockacct',
      address: '0xabc',
      balanceCredits: 0,
      lockedCredits: 0,
      totalSpentCredits: 0,
      createdAt: '2026-06-14',
      baseUrl: '',
    };
  }
  async keys(): Promise<Key[]> {
    return [];
  }
  async createKey(_label: string): Promise<CreatedKey> {
    throw new Error('not implemented');
  }
  async revokeKey(_id: string): Promise<void> {}
  async usage(_query?: UsageQuery): Promise<UsagePage> {
    return { rows: [], nextCursor: null };
  }
  async funding(_purchaseId: string): Promise<FundingStatus> {
    throw new Error('not implemented');
  }
  async fundingList(): Promise<FundingStatus[]> {
    return [];
  }
  async listings(_before?: string): Promise<ListingPage> {
    return { rows: [], nextCursor: null };
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

describe('ShellComponent', () => {
  // The store's stream gate is environment.useMock; keep it true so these
  // specs never open a real EventSource (jsdom has none).
  beforeAll(() => {
    environment.useMock = true;
  });

  let fixture: ComponentFixture<ShellComponent>;

  function setup() {
    TestBed.configureTestingModule({
      imports: [ShellComponent],
      providers: [
        provideRouter([
          { path: 'trade/:id', component: TradeDummyComponent },
          { path: 'connect', component: TradeDummyComponent },
        ]),
        { provide: SLOTFARE_API, useValue: new EmptyApi() },
        { provide: WalletService, useValue: makeFakeWallet() },
        ConsoleStore,
        BreakpointService,
      ],
    });
    fixture = TestBed.createComponent(ShellComponent);
    fixture.detectChanges();
  }

  function setBp(mode: 'mobile' | 'tablet' | 'desktop') {
    TestBed.inject(BreakpointService).setForTest(mode, true);
    fixture.detectChanges();
  }

  function el() {
    return fixture.nativeElement as HTMLElement;
  }

  function click(sel: string) {
    (el().querySelector(sel) as HTMLElement).click();
  }

  afterEach(() => TestBed.inject(ConsoleStore).stopPolling());

  it('the brand is a stacked square logo over the Slotfare wordmark', () => {
    setup();
    const brand = el().querySelector('.brand');
    expect(brand).toBeTruthy();
    // still a plain div: it never linked home, so no routerLink is added
    expect(brand!.tagName).toBe('DIV');

    const img = brand!.querySelector('img')!;
    expect(img.getAttribute('width')).toBe('56');
    expect(img.getAttribute('height')).toBe('56');
    expect(img.getAttribute('src')!.endsWith('slotfare-logo.svg')).toBe(true);

    const wordmark = brand!.querySelector('.wordmark')!;
    expect(wordmark.textContent).toBe('Slotfare');

    // DOM order: the logo precedes the wordmark among the brand's children
    const children = Array.from(brand!.children);
    expect(children[0]).toBe(img);
    expect(children[1]).toBe(wordmark);
  });

  it('wallet chip renders the truncated address, full value in the title', () => {
    setup();
    const chip = fixture.nativeElement.querySelector('.wallet-chip');
    const text = chip.textContent.trim();
    expect(text.startsWith('0x')).toBe(true);
    expect(text).not.toContain('[');
    expect(text).not.toContain('Computed');
    expect(text).toBe(SHORTENED_ADDRESS);
    expect(chip.getAttribute('title')).toBe(CONNECTED_ADDRESS);
  });

  it('the rail footer carries no stream badge (Live/Stale pill removed)', () => {
    setup();
    // Drive the old pill's gate true: connected session, stream open. If the
    // badge survived anywhere in the shell it would render now.
    const store = TestBed.inject(ConsoleStore);
    store.wallet.set({ name: 'Mock', rdns: 'mock.wallet', initial: 'M' });
    store.streamState.set('connected');
    fixture.detectChanges();

    const foot = el().querySelector('.rail-foot');
    expect(foot).toBeTruthy();
    expect(foot!.querySelector('.pill')).toBeNull();
    expect(foot!.textContent).not.toContain('Live');
    expect(foot!.textContent).not.toContain('Stale');
    // The retired nav badge style is gone from the whole shell DOM too.
    expect(el().querySelector('.nav-badge')).toBeNull();
  });

  it('titles a /trade/:id route as Trade detail', async () => {
    setup();
    const router = TestBed.inject(Router);
    await router.navigate(['/trade/7']);
    fixture.detectChanges();
    const h1 = fixture.nativeElement.querySelector('h1');
    expect(h1.textContent.trim()).toBe('Trade detail');
  });

  it('the Build nav carries a Models link to /models', () => {
    setup();
    const items = Array.from(el().querySelectorAll('a.nav-item'));
    const models = items.find((a) => a.textContent?.trim() === 'Models');
    expect(models).toBeDefined();
    expect(models!.getAttribute('href')).toBe('/models');
    // The retired Endpoint & pricing screen is no longer in the nav.
    expect(items.some((a) => a.textContent?.trim() === 'Endpoint & pricing')).toBe(
      false,
    );
  });

  it('the nav order: Models above API keys', () => {
    setup();
    const labels = Array.from(el().querySelectorAll('a.nav-item')).map(
      (a) => a.textContent?.trim(),
    );
    expect(labels).toEqual([
      'Overview',
      'Add credits',
      'Sell',
      'Market',
      'Transactions',
      'Models',
      'API keys',
      'Usage',
    ]);
    // Models sits directly above API keys.
    expect(labels.indexOf('API keys')).toBe(labels.indexOf('Models') + 1);
  });

  it('mobile: the drawer toggles behind the menu button and keeps the balance block', () => {
    setup();
    setBp('mobile');
    const menu = el().querySelector('.menu-btn') as HTMLElement;
    const rail = el().querySelector('.rail');
    expect(menu).toBeTruthy();
    expect(rail!.classList).not.toContain('drawer-open');
    expect(el().querySelector('.scrim')).toBeNull();

    menu!.click();
    fixture.detectChanges();
    expect(rail!.classList).toContain('drawer-open');
    expect(el().querySelector('.scrim')).toBeTruthy();
    // the balance block stays reachable inside the drawer
    expect(rail!.querySelector('.rail-foot')).toBeTruthy();

    click('.scrim');
    fixture.detectChanges();
    expect(rail!.classList).not.toContain('drawer-open');
    expect(el().querySelector('.scrim')).toBeNull();
  });

  it('tablet: reuses the same drawer form', () => {
    setup();
    setBp('tablet');
    const menu = el().querySelector('.menu-btn') as HTMLElement;
    const rail = el().querySelector('.rail');
    expect(menu).toBeTruthy();
    expect(rail!.classList).not.toContain('drawer-open');

    menu!.click();
    fixture.detectChanges();
    expect(rail!.classList).toContain('drawer-open');
    expect(el().querySelector('.scrim')).toBeTruthy();
    expect(rail!.querySelector('.rail-foot')).toBeTruthy();

    click('.scrim');
    fixture.detectChanges();
    expect(rail!.classList).not.toContain('drawer-open');
  });

  it('desktop: full sidebar, no menu button, no scrim', () => {
    setup();
    setBp('desktop');
    expect(el().querySelector('.menu-btn')).toBeNull();
    expect(el().querySelector('.scrim')).toBeNull();
    const rail = el().querySelector('.rail');
    expect(rail).toBeTruthy();
    expect(rail!.classList).not.toContain('drawer-open');
  });

  it('closes the drawer on Escape and on navigation', async () => {
    setup();
    setBp('mobile');
    click('.menu-btn');
    fixture.detectChanges();
    expect(el().querySelector('.rail')!.classList).toContain('drawer-open');

    el().dispatchEvent(new KeyboardEvent('keydown', { key: 'escape', bubbles: true }));
    fixture.detectChanges();
    expect(el().querySelector('.rail')!.classList).not.toContain('drawer-open');

    click('.menu-btn');
    fixture.detectChanges();
    expect(el().querySelector('.rail')!.classList).toContain('drawer-open');

    const router = TestBed.inject(Router);
    await router.navigate(['/trade/7']);
    fixture.detectChanges();
    expect(el().querySelector('.rail')!.classList).not.toContain('drawer-open');
  });

  it('wallet chip is a button whose textContent is the truncated address', () => {
    setup();
    const chip = el().querySelector('.wallet-chip');
    expect(chip!.tagName).toBe('BUTTON');
    expect(chip!.textContent.trim()).toBe(SHORTENED_ADDRESS);
    expect(chip!.getAttribute('title')).toBe(CONNECTED_ADDRESS);
  });

  it('disconnect is a secondary button at rest and still disconnects', () => {
    setup();
    // The component fires off a /connect navigation; the test route table has
    // no /connect, so stub navigate to keep the run free of unhandled errors.
    vi
      .spyOn(TestBed.inject(Router), 'navigate')
      .mockResolvedValue(true);
    const store = TestBed.inject(ConsoleStore);
    const spy = vi.spyOn(store, 'disconnect');
    const btn = Array.from(el().querySelectorAll('button'))
      .find((b) => b.textContent?.trim() === 'Disconnect')!;
    expect(btn.tagName).toBe('BUTTON');
    expect(btn.classList).toContain('btn');
    expect(btn.classList).toContain('btn-secondary');
    expect(btn.classList).not.toContain('btn-ghost');
    btn.click();
    fixture.detectChanges();
    expect(spy).toHaveBeenCalled();
    spy.mockRestore();
  });

  it('the overview subtitle shows the truncated credit account address', () => {
    setup();
    const sub = el().querySelector('.topbar-title .sub');
    expect(sub!.textContent.trim()).toBe(`Credit account ${SHORTENED_ADDRESS}`);
  });

  it('shows the recovery prompt only while sessionExpired', () => {
    setup();
    // No session yet: no prompt.
    expect(el().querySelector('.session-recovery')).toBeNull();

    const store = TestBed.inject(ConsoleStore);
    store.sessionExpired.set(true);
    fixture.detectChanges();
    const block = el().querySelector('.session-recovery');
    expect(block).toBeTruthy();
    expect(block!.textContent).toContain('Session ended');
    const primary = Array.from(block!.querySelectorAll('button'))
      .find((b) => b.textContent?.trim() === 'Sign in again')!;
    expect(primary.classList).toContain('btn-primary');

    store.sessionExpired.set(false);
    fixture.detectChanges();
    expect(el().querySelector('.session-recovery')).toBeNull();
  });

  it('Sign in again calls store.reauth()', () => {
    setup();
    const store = TestBed.inject(ConsoleStore);
    const spy = vi.spyOn(store, 'reauth').mockImplementation(() => {});
    store.sessionExpired.set(true);
    fixture.detectChanges();
    const btn = Array.from(el().querySelectorAll('.session-recovery button'))
      .find((b) => b.textContent?.trim() === 'Sign in again') as HTMLElement;
    btn.click();
    fixture.detectChanges();
    expect(spy).toHaveBeenCalled();
    spy.mockRestore();
  });

  it('abandoned recovery (prompt drops while signed out) navigates to /connect', async () => {
    setup();
    const router = TestBed.inject(Router);
    const navSpy = vi.spyOn(router, 'navigate').mockResolvedValue(true);
    const store = TestBed.inject(ConsoleStore);

    // Show the prompt, then drop it while still signed out: an abandoned
    // recovery must land on the sign-in screen.
    store.sessionExpired.set(true);
    fixture.detectChanges();
    store.sessionExpired.set(false);
    fixture.detectChanges();
    await tick();

    expect(navSpy).toHaveBeenCalledWith(['/connect']);
    navSpy.mockRestore();
  });
});
