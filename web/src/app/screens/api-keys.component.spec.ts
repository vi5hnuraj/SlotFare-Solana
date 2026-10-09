import { computed, signal } from '@angular/core';
import { vi } from 'vitest';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { provideRouter } from '@angular/router';
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
import { environment } from '../../environments/environment';
import { ApiKeysComponent } from './api-keys.component';

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
  /** The base URL the account read advertises. */
  accountBaseUrl = 'https://wire.example/v1';
  /** Mutable catalog returned by models(). */
  modelData: Model[] = [];
  /** When set, models() rejects so the catalog surfaces an error. */
  modelsFail = false;
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
      baseUrl: this.accountBaseUrl,
    };
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
    if (this.modelsFail) throw new ApiError(503, 'unavailable', 'nope');
    return { models: this.modelData.map((m) => ({ ...m })), baseUrl: '' };
  }

  /** EP-027-T05: the inclusion proof read; this screen does not exercise it. */
  async usageProof(_rowId: string): Promise<UsageProof> {
    throw new ApiError(500, 'internal', 'not implemented');
  }
}

const CHAT_MODEL: Model = {
  id: 'mistral-nemotron',
  name: 'Mistral Nemotron',
  vendor: 'Mistral',
  kind: 'chat',
  contextLength: 131072,
  parameters: '12B',
  servingPath: 'priority',
  description: 'A 12B reasoning model with strong tool use.',
  capabilities: ['function-calling'],
  pricing: { uncachedInput: 20, cachedInput: 2, output: 30 },
};

describe('ApiKeysComponent', () => {
  beforeAll(() => {
    environment.useMock = true;
  });
  let fixture: ComponentFixture<ApiKeysComponent>;
  let fakeApi: FakeApi;

  function setup(modelData: Model[], accountBaseUrl = 'https://wire.example/v1') {
    fakeApi = new FakeApi();
    fakeApi.modelData = modelData;
    fakeApi.accountBaseUrl = accountBaseUrl;
    TestBed.configureTestingModule({
      imports: [ApiKeysComponent],
      providers: [
        provideRouter([]),
        { provide: SLOTFARE_API, useValue: fakeApi },
        { provide: WalletService, useValue: makeFakeWallet() },
        ConsoleStore,
        ToastService,
      ],
    });
    fixture = TestBed.createComponent(ApiKeysComponent);
    fixture.detectChanges();
    const wallet = TestBed.inject(WalletService);
    wallet.address.set('0xabc');
    wallet.accountId.set('0xmockacct');
    wallet.token.set('mock-token');
    TestBed.flushEffects();
  }

  afterEach(() => TestBed.inject(ConsoleStore).stopPolling());

  function el(): HTMLElement {
    return fixture.nativeElement as HTMLElement;
  }

  it('renders the wire base URL and a first-request curl example', async () => {
    setup([CHAT_MODEL]);
    await tick();
    fixture.detectChanges();

    // The Base URL copy row carries the wire value, not the page origin.
    expect(el().querySelector('.copy-field')!.textContent?.trim()).toBe(
      'https://wire.example/v1',
    );
    // The curl example: wire URL, real model id, chat-shaped body, env-var header.
    const pre = el().querySelector('pre.code')!.textContent ?? '';
    expect(pre).toContain('curl https://wire.example/v1/chat/completions');
    expect(pre).toContain('"model": "mistral-nemotron"');
    expect(pre).toContain('Bearer $SLOTFARE_API_KEY');
    expect(pre).not.toContain('attk_');
    // One copy affordance per thing: the snippet's copy button sits in the
    // copy-row next to the well, not in the card head.
    const firstHead = Array.from(el().querySelectorAll('.card-head h3')).find(
      (h) => h.textContent?.trim() === 'First request',
    )!;
    const card = firstHead.closest('.card')!;
    expect(card.querySelector('.card-head slf-copy-button')).toBeNull();
    const blockBtn = card.querySelector('.copy-row .well')!.nextElementSibling;
    expect(blockBtn?.tagName.toLowerCase()).toBe('slf-copy-button');
    expect(blockBtn!.querySelector('button')!.getAttribute('aria-label')).toBe(
      'Copy curl example',
    );
    // Clicking it copies the exact snippet text; the runner has no
    // clipboard API, so install a fake for the call's duration.
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, 'clipboard', {
      value: { writeText },
      configurable: true,
    });
    blockBtn!.querySelector('button')!.click();
    fixture.detectChanges();
    expect(writeText).toHaveBeenCalledWith(pre);
    delete (navigator as { clipboard?: unknown }).clipboard;
    // Both copy buttons are present.
    expect(el().querySelectorAll('slf-copy-button').length).toBeGreaterThanOrEqual(2);
    // The caption carries the one-time setup instead of a replace instruction.
    expect(el().textContent).toContain('export SLOTFARE_API_KEY=attk_...');
    // The micro-credit metering note moved here verbatim.
    expect(el().textContent).toContain('x-slotfare-credits-charged');
    expect(el().textContent).toContain('x-slotfare-balance');
    expect(el().textContent).toContain('Streamed responses omit the charge header.');
  });

  it('shows the metered /rpc example when the wire advertises no inference base URL', async () => {
    setup([CHAT_MODEL], '');
    await tick();
    fixture.detectChanges();

    const expected = `${window.location.origin}/rpc`;
    expect(el().querySelector('.copy-field')!.textContent?.trim()).toBe(expected);
    const pre = el().querySelector('pre.code')!.textContent ?? '';
    expect(pre).toContain(`curl ${expected}`);
    expect(pre).toContain('"method":"getHealth"');
    expect(pre).toContain('Bearer $SLOTFARE_API_KEY');
    // No inference example is offered while the upstream is off.
    expect(pre).not.toContain('/chat/completions');
    expect(el().textContent).toContain('Inference isn\'t configured on this deployment');
  });

  it('carries the placeholder model id with a chat body when the catalog is unavailable', async () => {
    fakeApi = new FakeApi();
    fakeApi.modelsFail = true;
    TestBed.configureTestingModule({
      imports: [ApiKeysComponent],
      providers: [
        provideRouter([]),
        { provide: SLOTFARE_API, useValue: fakeApi },
        { provide: WalletService, useValue: makeFakeWallet() },
        ConsoleStore,
        ToastService,
      ],
    });
    fixture = TestBed.createComponent(ApiKeysComponent);
    fixture.detectChanges();
    const wallet = TestBed.inject(WalletService);
    wallet.address.set('0xabc');
    wallet.accountId.set('0xmockacct');
    wallet.token.set('mock-token');
    TestBed.flushEffects();
    await tick();
    fixture.detectChanges();

    const pre = el().querySelector('pre.code')!.textContent ?? '';
    expect(pre).toContain('"model": "<model-id>"');
    expect(pre).toContain('"messages"');
    // The placeholder hint shows while the catalog is unavailable.
    expect(el().textContent).toContain(
      'Showing a placeholder model id while the catalog is unavailable.',
    );
  });
});
