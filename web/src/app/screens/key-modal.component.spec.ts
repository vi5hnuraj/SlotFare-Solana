import { computed, signal } from '@angular/core';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { ConsoleStore } from '../core/console-store';
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
import { KeyModalComponent } from './key-modal.component';

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

/** Fake API whose createKey resolves to a fixed plaintext. */
class FakeApi implements SlotfareApi {
  createCalls = 0;
  createResult: Promise<CreatedKey>;
  constructor(plaintext = 'slf_live_test…abcd') {
    this.createResult = Promise.resolve({
      key: {
        id: '7Qk4',
        label: 'prod-router',
        createdAt: '2026-08-22',
        lastUsedAt: null,
        requestCount: 0,
        revoked: false,
      },
      plaintext,
    });
  }

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
      balanceCredits: 100,
      lockedCredits: 0,
      totalSpentCredits: 0,
      createdAt: '2026-06-14',
      baseUrl: '',
    };
  }
  async keys(): Promise<Key[]> {
    return [
      {
        id: '7Qk4',
        label: 'prod-router',
        createdAt: '2026-08-22',
        lastUsedAt: null,
        requestCount: 0,
        revoked: false,
      },
    ];
  }
  async createKey(_label: string): Promise<CreatedKey> {
    this.createCalls += 1;
    return this.createResult;
  }
  async revokeKey(_id: string): Promise<void> {}
  async usage(_query?: UsageQuery): Promise<UsagePage> {
    return { rows: [], nextCursor: null };
  }
  async funding(_purchaseId: string): Promise<FundingStatus> {
    throw new ApiError(500, 'internal', 'no');
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

describe('KeyModalComponent', () => {
  let fixture: ComponentFixture<KeyModalComponent>;
  let fakeApi: FakeApi;

  function setup() {
    fakeApi = new FakeApi();
    TestBed.configureTestingModule({
      imports: [KeyModalComponent],
      providers: [
        { provide: SLOTFARE_API, useValue: fakeApi },
        { provide: WalletService, useValue: makeFakeWallet() },
        ConsoleStore,
        ToastService,
      ],
    });
    fixture = TestBed.createComponent(KeyModalComponent);
    fixture.detectChanges();
  }

  afterEach(() => TestBed.inject(ConsoleStore).stopPolling());

  /** Open the dialog and submit the create form. */
  async function create() {
    fixture.componentInstance.show();
    fixture.detectChanges();
    const input = fixture.nativeElement.querySelector('#keyName');
    input.value = 'prod-router';
    input.dispatchEvent(new Event('input'));
    fixture.detectChanges();
    const buttons = Array.from(
      fixture.nativeElement.querySelectorAll('.modal-foot button'),
    ) as HTMLButtonElement[];
    const createBtn = buttons.find((b) => b.textContent?.includes('Create key'));
    createBtn!.click();
    await tick();
    fixture.detectChanges();
  }

  it('create renders exactly what the server returned', async () => {
    setup();
    await create();
    expect(fakeApi.createCalls).toBe(1);
    const keyEl = fixture.nativeElement.querySelector('.reveal .key');
    expect(keyEl.textContent.trim()).toBe('slf_live_test…abcd');
  });

  it('renders inside the viewport-bound modal overlay', () => {
    setup();
    fixture.componentInstance.show();
    fixture.detectChanges();

    const scrim = fixture.nativeElement.querySelector('.modal-scrim') as HTMLElement;
    expect(scrim).toBeTruthy();
    expect(scrim.getAttribute('role')).toBe('dialog');
    const modal = scrim.querySelector(':scope > .modal');
    expect(modal).toBeTruthy();
  });

  it('the plaintext is absent after close', async () => {
    setup();
    await create();
    const plaintext = 'slf_live_test…abcd';
    expect(fixture.nativeElement.innerHTML).toContain(plaintext);

    // Check the "stored" box and click Done to close.
    const checkbox = fixture.nativeElement.querySelector('input[type="checkbox"]');
    checkbox.checked = true;
    checkbox.dispatchEvent(new Event('change'));
    fixture.detectChanges();
    const doneBtn = Array.from(
      fixture.nativeElement.querySelectorAll('.modal-foot button'),
    ) as HTMLButtonElement[];
    const done = doneBtn.find((b) => b.textContent?.trim() === 'Done');
    done!.click();
    await tick();
    fixture.detectChanges();

    expect(fixture.nativeElement.innerHTML).not.toContain(plaintext);
  });

  it('a failed create surfaces an error and does not reveal', async () => {
    fakeApi = new FakeApi();
    fakeApi.createResult = Promise.reject(new ApiError(500, 'internal', 'boom'));
    TestBed.configureTestingModule({
      imports: [KeyModalComponent],
      providers: [
        { provide: SLOTFARE_API, useValue: fakeApi },
        { provide: WalletService, useValue: makeFakeWallet() },
        ConsoleStore,
        ToastService,
      ],
    });
    fixture = TestBed.createComponent(KeyModalComponent);
    fixture.detectChanges();

    fixture.componentInstance.show();
    fixture.detectChanges();
    const input = fixture.nativeElement.querySelector('#keyName');
    input.value = 'prod-router';
    input.dispatchEvent(new Event('input'));
    fixture.detectChanges();
    const createBtn = (Array.from(
      fixture.nativeElement.querySelectorAll('.modal-foot button'),
    ) as HTMLButtonElement[]).find((b) => b.textContent?.includes('Create key'));
    createBtn!.click();
    await tick();
    fixture.detectChanges();

    expect(fixture.nativeElement.querySelector('.reveal')).toBeNull();
    expect(fixture.nativeElement.innerHTML).toContain('boom');
  });
});
