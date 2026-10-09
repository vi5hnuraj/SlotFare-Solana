import { computed, signal } from '@angular/core';
import { readFileSync } from 'node:fs';
import { Component } from '@angular/core';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { provideRouter, ActivatedRoute, Router, RouterOutlet } from '@angular/router';
import { of } from 'rxjs';
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
import { BreakpointService } from '../core/breakpoint';
import { ModelDetailComponent } from './model-detail.component';

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
    return { models: this.modelData.map((m) => ({ ...m })), baseUrl: '' };
  }

  /** EP-027-T05: the inclusion proof read; this screen does not exercise it. */
  async usageProof(_rowId: string): Promise<UsageProof> {
    throw new ApiError(500, 'internal', 'not implemented');
  }
}

const RICH_MODEL: Model = {
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

const TESTNET_MODEL: Model = {
  id: 'llama-3.2-3b',
  name: 'Llama 3.2 3B',
  pricing: { uncachedInput: 10, cachedInput: 1, output: 40 },
};

/** The stub the back control navigates to; proves the route resolves. */
@Component({ template: '<div>STUB-LIST</div>' })
class StubListComponent {}

/** The test root: hosts both the detail view and the routed outlet. */
@Component({ template: '<slf-model-detail /><router-outlet />', imports: [RouterOutlet, ModelDetailComponent] })
class RootComponent {}

describe('ModelDetailComponent', () => {
  beforeAll(() => {
    environment.useMock = true;
  });
  let fixture: ComponentFixture<RootComponent>;
  let fakeApi: FakeApi;

  function setup(modelData: Model[], id: string | null, accountBaseUrl = 'https://wire.example/v1') {
    fakeApi = new FakeApi();
    fakeApi.modelData = modelData;
    fakeApi.accountBaseUrl = accountBaseUrl;
    TestBed.configureTestingModule({
      imports: [ModelDetailComponent, RootComponent],
      providers: [
        provideRouter([{ path: 'models', component: StubListComponent }]),
        { provide: SLOTFARE_API, useValue: fakeApi },
        { provide: WalletService, useValue: makeFakeWallet() },
        ConsoleStore,
        ToastService,
        {
          provide: ActivatedRoute,
          useValue: {
            paramMap: of({ get: () => id }),
            snapshot: { paramMap: { get: () => id } },
          },
        },
      ],
    });
    fixture = TestBed.createComponent(RootComponent);
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

  /** The card whose head is the given title; the page carries more than one. */
  function card(title: string): HTMLElement {
    const h = Array.from(el().querySelectorAll('h3')).find(
      (x) => x.textContent?.trim() === title,
    )!;
    return h.closest('.card')!;
  }

  it('pins the Example Base URL to the wire value on this surface', async () => {
    setup([RICH_MODEL], 'mistral-nemotron');
    await tick();
    fixture.detectChanges();

    // The Example card's Base URL copy row carries the wire value exactly.
    const example = card('Example');
    expect(example.querySelector('.copy-field')!.textContent?.trim()).toBe(
      'https://wire.example/v1',
    );
    // The curl builds from the same wire base URL, never the page origin.
    const pre = example.querySelector('pre.code')!.textContent ?? '';
    expect(pre).toContain('curl https://wire.example/v1/chat/completions');
    expect(pre).not.toContain(`${window.location.origin}/v1/chat/completions`);
  });

  it('shows an /rpc note instead of the curl example when the wire advertises no inference base URL', async () => {
    setup([RICH_MODEL], 'mistral-nemotron', '');
    await tick();
    fixture.detectChanges();

    const example = card('Example');
    // No Base URL row and no dead curl; the note points at API keys instead.
    expect(example.querySelector('.copy-field')).toBeNull();
    expect(example.querySelector('pre.code')).toBeNull();
    expect(example.textContent).toContain(
      "Inference isn't configured on this deployment",
    );
    expect(example.textContent).toContain('the API keys screen');
  });

  it('puts Details and Pricing side by side on desktop, the Example full width below', async () => {
    setup([RICH_MODEL], 'mistral-nemotron');
    await tick();
    fixture.detectChanges();
    const detail = el().querySelector('.model-detail')!;
    const facts = detail.querySelector('.split.model-detail-facts')!;
    expect(facts).toBeTruthy();

    TestBed.inject(BreakpointService).setForTest('desktop', false);
    fixture.detectChanges();
    expect(facts.classList).not.toContain('cols-1');
    // Two cards in the facts row, Details then Pricing.
    const factCards = Array.from(facts.querySelectorAll(':scope > .card'));
    expect(factCards).toHaveLength(2);
    expect(factCards[0].querySelector('h3')!.textContent?.trim()).toBe('Details');
    expect(factCards[1].querySelector('h3')!.textContent?.trim()).toBe('Pricing');
    // The Example card is the detail's own second child, spanning the full width.
    const rows = Array.from(detail.children);
    expect(rows).toHaveLength(2);
    expect(rows[1].querySelector('h3')!.textContent?.trim()).toBe('Example');

    TestBed.inject(BreakpointService).setForTest('tablet', true);
    fixture.detectChanges();
    expect(facts.classList).toContain('cols-1');
  });

  it('renders the new wire facts when present and omits them when absent', async () => {
    setup(
      [
        {
          id: 'full-facts',
          name: 'Full Facts',
          contextLength: 262144,
          moe: true,
          capabilities: ['text', 'image', 'function-calling'],
          pricing: { uncachedInput: 10, cachedInput: 1, output: 40 },
        },
      ],
      'full-facts',
    );
    await tick();
    fixture.detectChanges();

    const kv = Array.from(el().querySelectorAll('.kv-list .kv')).map((k) =>
      k.textContent?.trim(),
    );
    expect(kv).toContain('Context window262k');
    expect(kv).toContain('Mixture of expertsYes');
    // Capabilities sit in the head's chip row after the kind and serving-path chips.
    const capBadges = Array.from(el().querySelectorAll('.model-detail-id .cap-badge')).map(
      (b) => b.textContent?.trim(),
    );
    expect(capBadges).toEqual(['Chat', 'Standard', 'Text', 'Image', 'Function calling']);
  });

  it('renders an explicit moe false as a No row', async () => {
    setup(
      [
        {
          id: 'dense-9b',
          name: 'Dense 9B',
          moe: false,
          pricing: { uncachedInput: 10, cachedInput: 1, output: 40 },
        },
      ],
      'dense-9b',
    );
    await tick();
    fixture.detectChanges();

    const kv = Array.from(el().querySelectorAll('.kv-list .kv')).map((k) =>
      k.textContent?.trim(),
    );
    expect(kv).toContain('Mixture of expertsNo');
    expect(kv).not.toContain('Context window');
  });

  it('renders the wire metadata for a full model', async () => {
    setup([RICH_MODEL], 'mistral-nemotron');
    await tick();
    fixture.detectChanges();

    expect(el().querySelector('h2')!.textContent?.trim()).toBe('Mistral Nemotron');
    // The mono id line with its copy button.
    expect(el().querySelector('.model-detail-id .u-mono')!.textContent?.trim()).toBe(
      'mistral-nemotron',
    );
    expect(el().querySelector('.model-detail-id slf-copy-button')).not.toBeNull();
    // Kind and serving-path badges use the wire values.
    const badges = Array.from(el().querySelectorAll('.model-detail-id span')).map(
      (b) => b.textContent?.trim(),
    );
    expect(badges).toContain('Chat');
    expect(badges).toContain('Priority');
    // Details card: context window and parameters from the wire.
    const kv = Array.from(el().querySelectorAll('.kv-list .kv')).map((k) =>
      k.textContent?.trim(),
    );
    expect(kv).toContain('Context window131k');
    expect(kv).toContain('Parameters12B');
    // Capability badge and the full description.
    const capBadges = Array.from(el().querySelectorAll('.cap-badge')).map(
      (b) => b.textContent?.trim(),
    );
    expect(capBadges).toContain('Function calling');
    expect(el().textContent).toContain('A 12B reasoning model with strong tool use.');
  });

  it('renders the complete price table in the required row order', async () => {
    setup([RICH_MODEL], 'mistral-nemotron');
    await tick();
    fixture.detectChanges();

    const pricing = Array.from(el().querySelectorAll('h3'))
      .find((h) => h.textContent?.trim() === 'Pricing')!
      .closest('.card')!;
    const labels = Array.from(pricing.querySelectorAll('tbody tr td:first-child')).map(
      (td) => td.textContent?.trim(),
    );
    expect(labels).toEqual(['Uncached input', 'Cached input', 'Output']);
    const credits = Array.from(pricing.querySelectorAll('tbody tr td.num')).map(
      (td) => td.textContent?.trim(),
    );
    expect(credits).toEqual(['20', '2', '30']);
  });

  it('builds the curl example from the wire base URL and the model kind', async () => {
    setup([RICH_MODEL], 'mistral-nemotron');
    await tick();
    fixture.detectChanges();

    const pre = el().querySelector('pre.code')!.textContent ?? '';
    expect(pre).toContain('curl https://wire.example/v1/chat/completions');
    expect(pre).toContain('"model": "mistral-nemotron"');
    expect(pre).toContain('Bearer $SLOTFARE_API_KEY');
    expect(pre).not.toContain('attk_');
    // The base URL copy row carries the same wire value.
    expect(el().querySelector('.copy-field')!.textContent?.trim()).toBe(
      'https://wire.example/v1',
    );
    // The caption carries the one-time setup; the link survives in it.
    expect(el().textContent).toContain('export SLOTFARE_API_KEY=attk_...');
    expect(el().querySelector('a[routerlink="/keys"]')).not.toBeNull();
    // One copy affordance per thing: the snippet's copy button sits in the
    // copy-row next to the well, not in the card head.
    const exampleHead = Array.from(el().querySelectorAll('.card-head h3')).find(
      (h) => h.textContent?.trim() === 'Example',
    )!;
    const card = exampleHead.closest('.card')!;
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
  });

  it('builds an embeddings-shaped example for an embeddings model', async () => {
    setup(
      [{ id: 'jina-v3-embeddings', name: 'Jina v3 Embeddings', kind: 'embeddings', pricing: { uncachedInput: 50, cachedInput: 0, output: 0 } }],
      'jina-v3-embeddings',
    );
    await tick();
    fixture.detectChanges();

    const pre = el().querySelector('pre.code')!.textContent ?? '';
    expect(pre).toContain('curl https://wire.example/v1/embeddings');
    expect(pre).toContain('"model": "jina-v3-embeddings"');
    expect(pre).toContain('"input"');
    expect(pre).not.toContain('"messages"');
  });

  it('builds a reranker-shaped example for a reranker model', async () => {
    setup(
      [{ id: 'rerank-v1', name: 'Slotfare Reranker v1', kind: 'reranker', pricing: { uncachedInput: 20, cachedInput: 0, output: 0 } }],
      'rerank-v1',
    );
    await tick();
    fixture.detectChanges();

    const pre = el().querySelector('pre.code')!.textContent ?? '';
    expect(pre).toContain('curl https://wire.example/v1/rerank');
    expect(pre).toContain('"model": "rerank-v1"');
    expect(pre).toContain('"query"');
    expect(pre).toContain('"documents"');
    expect(pre).not.toContain('"messages"');
  });

  it('a bare testnet model reads complete, not sparse', async () => {
    setup([TESTNET_MODEL], 'llama-3.2-3b');
    await tick();
    fixture.detectChanges();

    // Name, id and the documented defaults still render.
    expect(el().querySelector('h2')!.textContent?.trim()).toBe('Llama 3.2 3B');
    const badges = Array.from(el().querySelectorAll('.model-detail-id span')).map(
      (b) => b.textContent?.trim(),
    );
    expect(badges).toContain('Chat');
    expect(badges).toContain('Standard');
    // No description paragraph under the head, no absent kv rows or badges.
    const head = el().querySelector('.page-head')!;
    expect(head.querySelector('p')).toBeNull();
    const kv = Array.from(el().querySelectorAll('.kv-list .kv')).map((k) =>
      k.textContent?.trim(),
    );
    expect(kv).toEqual(['KindChat', 'Serving pathStandard']);
    // No capability badges: the wire carried none.
    expect(el().querySelector('.model-line .cap-badge')).toBeNull();
    // Yet the pricing table and the curl example are present and complete.
    expect(el().querySelectorAll('tbody tr').length).toBe(3);
    const pre = el().querySelector('pre.code')!.textContent ?? '';
    expect(pre).toContain('"model": "llama-3.2-3b"');
  });

  it('an unknown id shows the empty block with a link back to /models', async () => {
    setup([TESTNET_MODEL], 'nope');
    await tick();
    fixture.detectChanges();

    expect(el().textContent).toContain('Unknown model');
    expect(el().textContent).toContain('nope');
    expect(el().querySelector('a[routerlink="/models"]')).not.toBeNull();
    // No detail cards rendered.
    expect(el().querySelector('.model-detail')).toBeNull();
  });

  it('the back control navigates to the models list', async () => {
    setup([TESTNET_MODEL], 'llama-3.2-3b');
    await tick();
    fixture.detectChanges();

    const back = el().querySelector<HTMLAnchorElement>('.model-detail-back')!;
    expect(back).not.toBeNull();
    expect(back.getAttribute('href')).toBe('/models');
    back.click();
    await tick();
    fixture.detectChanges();
    expect(TestBed.inject(Router).url).toBe('/models');
    // The routed list stub rendered in place of the detail view.
    expect(el().textContent).toContain('STUB-LIST');
    expect(el().textContent).not.toContain('Mistral Nemotron');
  });

  it('a known id syncs the store selection', async () => {
    setup([TESTNET_MODEL, RICH_MODEL], 'mistral-nemotron');
    await tick();
    fixture.detectChanges();
    TestBed.flushEffects();

    const store = TestBed.inject(ConsoleStore);
    expect(store.selectedModelId()).toBe('mistral-nemotron');
  });
});

describe('ModelDetailComponent styles', () => {
  it('keeps the curl block inside its card by letting the well shrink', () => {
    // jsdom does no layout, so the shipped stylesheet is the claim: the
    // well must be a shrinkable flex child and pre.code must scroll.
    const cssPath = import.meta.url
      .replace(/app\/screens\/[^/]+$/, 'styles/slotfare.scss')
      .replace(/^file:\/\//, '');
    const css = readFileSync(cssPath, 'utf8');

    const wellRule = css.match(/\.copy-row > \.well\s*\{[^}]*\}/)![0];
    expect(wellRule).toContain('flex: 1');
    expect(wellRule).toContain('min-width: 0');
    const codeRule = css.match(/pre\.code\s*\{[^}]*\}/)![0];
    expect(codeRule).toContain('overflow-x: auto');
  });
});
