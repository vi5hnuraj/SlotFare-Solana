import { Service, computed, signal, inject } from '@angular/core';
import type { Transaction } from '@solana/web3.js';
import { Wallet } from './models';
import { API_BASE_URL } from './runtime-config';

/**
 * Minimal structural type for a provider that answers `request`. Kept so
 * the (quarantined) EVM screens keep type-checking; the live sign-in path
 * never calls it.
 */
export interface Eip1193LikeProvider {
  request(args: { method: string; params?: unknown }): Promise<unknown>;
  on?(event: string, handler: (...args: unknown[]) => void): void;
  removeListener?(event: string, handler: (...args: unknown[]) => void): void;
}

/** The injected Solana wallet provider (Phantom and friends). */
interface SolanaProvider {
  isPhantom?: boolean;
  isConnected?: boolean;
  publicKey?: { toBase58(): string } | null;
  connect(opts?: { onlyIfTrusted?: boolean }): Promise<{ publicKey: { toBase58(): string } }>;
  disconnect?(): Promise<void>;
  signMessage(
    message: Uint8Array,
    display?: 'utf8' | 'hex',
  ): Promise<{ signature: Uint8Array; publicKey?: { toBase58(): string } }>;
  /** Sign a prepared transaction (Phantom's `signTransaction`). Absent on
   *  wallets that only support message signing; the transfer paths then
   *  fail with an explicit message instead of a crash. */
  signTransaction?(tx: Transaction): Promise<Transaction>;
  on?(event: string, handler: (...args: unknown[]) => void): void;
  removeListener?(event: string, handler: (...args: unknown[]) => void): void;
}

/** A verified sign-in session: the wallet address and its derived identity. */
export interface ConsoleSession {
  address: string;
  accountId: string;
  token: string;
}

/** Response of `POST /console/session/nonce`. */
interface NonceResponse {
  nonce: string;
  statement: string;
  domain: string;
  uri: string;
  issuedAt: string;
  expiresAt: string;
}

/** Response of `POST /console/session/verify` on success. */
interface VerifyResponse {
  accountId: string;
  token: string;
  expiresAt: string;
}

/** Error body used by both session endpoints. */
interface ApiErrorBody {
  error?: string;
}

/** Parse the `{"error": ...}` body of a failed session response. */
async function apiError(res: Response): Promise<Error> {
  let msg = `Request failed (${res.status})`;
  try {
    const body = (await res.json()) as ApiErrorBody;
    if (typeof body.error === 'string' && body.error.length > 0) {
      msg = body.error;
    }
  } catch {
    // Non-JSON error body: keep the status-based message.
  }
  return new Error(msg);
}

const BASE58_ALPHABET = '123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz';

/** Decode a base58 string to its bytes; throws on an invalid character. */
export function base58Decode(s: string): Uint8Array {
  let n = 0n;
  for (const ch of s) {
    const v = BASE58_ALPHABET.indexOf(ch);
    if (v < 0) throw new Error(`invalid base58 character: ${ch}`);
    n = n * 58n + BigInt(v);
  }
  const bytes: number[] = [];
  while (n > 0n) {
    bytes.unshift(Number(n & 0xffn));
    n >>= 8n;
  }
  for (const ch of s) {
    if (ch !== '1') break;
    bytes.unshift(0);
  }
  return Uint8Array.from(bytes);
}

/**
 * The account id is a pure function of the wallet address: 0x-hex of the
 * raw 32-byte public key - exactly what the server derives from the signed
 * public key and exactly what the deposit watcher credits, so one wallet
 * is one ledger row everywhere.
 */
export function computeAccountId(address: string): string {
  const bytes = base58Decode(address);
  if (bytes.length !== 32) {
    throw new Error(`expected a 32-byte public key, got ${bytes.length} bytes`);
  }
  return '0x' + Array.from(bytes, (b) => b.toString(16).padStart(2, '0')).join('');
}

/** Build the sign-in message byte-exact, as the server will rebuild it.
 *  Domain and URI come from the nonce response so client and server can
 *  never disagree on framing. */
function buildSignInMessage(address: string, nonce: NonceResponse): string {
  return [
    `${nonce.domain} wants you to sign in with your Solana account:`,
    address,
    '',
    nonce.statement,
    '',
    `URI: ${nonce.uri}`,
    'Version: 1',
    `Nonce: ${nonce.nonce}`,
    `Issued At: ${nonce.issuedAt}`,
    `Expiration Time: ${nonce.expiresAt}`,
  ].join('\n');
}

/** Session endpoints, relative to the console API base URL. */
const NONCE_PATH = '/console/session/nonce';
const VERIFY_PATH = '/console/session/verify';

/**
 * sessionStorage key for the durable session. sessionStorage is deliberately
 * used over localStorage: a page refresh restores the session silently,
 * while closing the tab still ends it - correct for shared/public
 * computers. Never localStorage.
 */
const SESSION_STORAGE_KEY = 'slotfare.console.session';

/** The raw sessionStorage payload for a verified session. */
interface StoredSession {
  token: string;
  address: string;
  accountId: string;
  rdns: string | null;
  wallet: Wallet | null;
}

/** Read the active sessionStorage object, or null when unavailable. */
function sessionStore(): Storage | null {
  try {
    return window.sessionStorage;
  } catch {
    return null;
  }
}

/**
 * Restore a session written by a previous page load. Every field is
 * type-checked; anything malformed is dropped and the storage key cleared
 * so a bad payload can never leak into the live session.
 */
function readStoredSession(): StoredSession | null {
  const store = sessionStore();
  if (store === null) return null;
  let raw: string | null = null;
  try {
    raw = store.getItem(SESSION_STORAGE_KEY);
  } catch {
    return null;
  }
  if (raw === null) return null;
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch {
    store.removeItem(SESSION_STORAGE_KEY);
    return null;
  }
  if (typeof parsed !== 'object' || parsed === null) {
    store.removeItem(SESSION_STORAGE_KEY);
    return null;
  }
  const p = parsed as Record<string, unknown>;
  const { token, address, accountId, rdns } = p;
  if (
    typeof token !== 'string' || token === '' ||
    typeof address !== 'string' || address === '' ||
    typeof accountId !== 'string' || accountId === ''
  ) {
    store.removeItem(SESSION_STORAGE_KEY);
    return null;
  }
  let wallet: Wallet | null = null;
  const w = p['wallet'];
  if (typeof w === 'object' && w !== null) {
    const wc = w as Record<string, unknown>;
    if (
      typeof wc['name'] === 'string' &&
      typeof wc['rdns'] === 'string' &&
      typeof wc['initial'] === 'string'
    ) {
      wallet = { name: wc['name'], rdns: wc['rdns'], initial: wc['initial'] };
      if (typeof wc['icon'] === 'string') wallet.icon = wc['icon'];
    }
  }
  return {
    token,
    address,
    accountId,
    rdns: typeof rdns === 'string' ? rdns : null,
    wallet,
  };
}

/** Persist a verified session to sessionStorage. Never writes localStorage. */
function writeStoredSession(payload: StoredSession): void {
  const store = sessionStore();
  if (store === null) return;
  try {
    store.setItem(SESSION_STORAGE_KEY, JSON.stringify(payload));
  } catch {
    // A full/blocked store is not fatal: the in-memory session still works.
  }
}

function removeStoredSession(): void {
  const store = sessionStore();
  if (store === null) return;
  try {
    store.removeItem(SESSION_STORAGE_KEY);
  } catch {
    // Nothing to do.
  }
}

/** The injected provider, if a supported Solana wallet announced itself. */
function findInjectedProvider(): SolanaProvider | null {
  const w = window as unknown as Record<string, unknown>;
  for (const key of ['solana', 'solflare']) {
    const p = w[key] as SolanaProvider | undefined;
    if (p && typeof p.signMessage === 'function' && typeof p.connect === 'function') {
      return p;
    }
  }
  return null;
}

/** Wallet sign-in and the verified console session. */
@Service()
export class WalletService {
  /** Console API base; runtime config wins over the compiled default. */
  private readonly baseUrl = inject(API_BASE_URL);
  readonly providers = signal<Wallet[]>([]);
  readonly address = signal<string | null>(null);
  /** No chain switching on Solana; kept as a signal for existing callers. */
  readonly chainId = signal<string | null>(null);
  readonly connecting = signal(false);
  readonly error = signal<string | null>(null);

  // ---- session -------------------------------------------------------
  /** Credit account id; null until the sign-in handshake succeeds. */
  readonly accountId = signal<string | null>(null);
  /**
   * Bearer token for the metering API. Durable across a page refresh via
   * sessionStorage; a tab close still ends it (never localStorage).
   */
  readonly token = signal<string | null>(null);
  /** The rdns of the wallet a restored session came from, if any. */
  readonly restoredRdns = signal<string | null>(null);
  /** The wallet entry a restored session came from, if any. */
  readonly restoredWallet = signal<Wallet | null>(null);
  /** Null until verified; the rest of the console reads it from here. */
  readonly session = computed<ConsoleSession | null>(() => {
    const t = this.token();
    if (t === null) return null;
    const addr = this.address();
    const id = this.accountId();
    if (addr === null || id === null) return null;
    return { address: addr, accountId: id, token: t };
  });

  /**
   * The connected provider, typed for the legacy EVM screens (which are
   * quarantined behind removed routes and never run). Non-null exactly
   * while a Solana wallet is connected, which the store's recovery flow
   * reads as a liveness signal.
   */
  get provider(): Eip1193LikeProvider | null {
    if (this.connectedProvider === null) return null;
    return {
      request: () =>
        Promise.reject(
          new Error('This build settles on Solana; EVM requests are not supported.'),
        ),
    };
  }

  /** Internal handle to the injected Solana provider. */
  private injectedProvider: SolanaProvider | null = null;
  private connectedProvider: SolanaProvider | null = null;
  private connectedRdns: string | null = null;
  private accountsHandler: ((...args: unknown[]) => void) | null = null;
  private disconnectHandler: (() => void) | null = null;

  constructor() {
    this.restoreSession();
    this.discover();
  }

  /**
   * Restore a session persisted to sessionStorage by a previous page load.
   * Populates the live session signals so a refresh silently resumes.
   */
  private restoreSession(): void {
    const stored = readStoredSession();
    if (stored === null) return;
    this.token.set(stored.token);
    this.accountId.set(stored.accountId);
    this.address.set(stored.address);
    this.restoredRdns.set(stored.rdns);
    this.restoredWallet.set(stored.wallet);
  }

  /** Persist the just-verified session so a page refresh restores it. */
  private persistSession(): void {
    const rdns = this.connectedRdns;
    const wallet = rdns !== null
      ? this.providers().find((w) => w.rdns === rdns) ?? null
      : null;
    writeStoredSession({
      token: this.token() ?? '',
      address: this.address() ?? '',
      accountId: this.accountId() ?? '',
      rdns,
      wallet,
    });
  }

  /**
   * Discover the injected Solana provider. Phantom injects at document
   * start, but other wallets can arrive a tick late, so discovery retries
   * briefly before giving up (the connect screen then offers the install
   * hint).
   */
  private discover(): void {
    const attempt = (triesLeft: number): void => {
      const provider = findInjectedProvider();
      if (provider !== null) {
        this.addProvider(provider);
        return;
      }
      if (triesLeft > 0) {
        setTimeout(() => attempt(triesLeft - 1), 400);
      }
    };
    attempt(5);
  }

  private addProvider(provider: SolanaProvider): void {
    if (this.injectedProvider !== null) return;
    this.injectedProvider = provider;
    const name = provider.isPhantom ? 'Phantom' : 'Solana Wallet';
    const rdns = provider.isPhantom ? 'app.phantom' : 'injected';
    const entry: Wallet = { name, rdns, initial: name.charAt(0).toUpperCase() };
    this.providers.update((list) => [...list, entry]);
    // Silent adoption after a refresh: a restored session plus an injected
    // provider means a later in-place re-sign has a target without a click.
    if (this.connectedProvider === null && this.session() !== null) {
      this.attachListeners(provider, rdns);
    }
  }

  /**
   * Connect to the injected provider identified by `rdns`. The only
   * providers published are the ones this service injected itself, so an
   * unknown rdns is a programming error, not a user state.
   */
  connect(rdns: string): void {
    const key = rdns.toLowerCase().trim();
    const provider = this.injectedProvider;
    if (provider === null) {
      this.error.set('No Solana wallet found - install Phantom and reload');
      return;
    }

    this.connecting.set(true);
    this.error.set(null);

    provider
      .connect()
      .then((res) => {
        const addr = res.publicKey.toBase58();
        this.address.set(addr);
        this.attachListeners(provider, key);
      })
      .catch((err: unknown) => {
        if (this.isUserCancellation(err)) {
          this.address.set(null);
          this.error.set(null);
        } else {
          this.address.set(null);
          const msg = err instanceof Error ? err.message : String(err);
          this.error.set(`Connection failed: ${msg}`);
        }
      })
      .finally(() => {
        this.connecting.set(false);
      });
  }

  /** User cancellations (Phantom code 4001 or a reject message). */
  private isUserCancellation(err: unknown): boolean {
    const e = err as { code?: unknown; message?: unknown };
    if (e.code === 4001) return true;
    return (
      typeof e.message === 'string' &&
      /denied|reject|cancel|closed/i.test(e.message)
    );
  }

  /** Attach the account-change listener to the given provider. */
  private attachListeners(provider: SolanaProvider, rdns: string): void {
    this.detachListeners();

    this.connectedProvider = provider;
    this.connectedRdns = rdns;

    if (!provider.on) return;
    this.accountsHandler = (...args: unknown[]): void => {
      const detail = args[0] as { publicKey?: { toBase58(): string } | null } | undefined;
      const next = detail?.publicKey?.toBase58() ?? null;
      if (next === null || next !== this.address()) {
        // No wallet, or a different wallet: a different customer.
        this.clearSession();
      }
    };
    provider.on('accountChanged', this.accountsHandler);
    this.disconnectHandler = (): void => this.clearSession();
    provider.on('disconnect', this.disconnectHandler);
  }

  private detachListeners(): void {
    const p = this.connectedProvider;
    if (p?.removeListener) {
      if (this.accountsHandler) {
        p.removeListener('accountChanged', this.accountsHandler);
      }
      if (this.disconnectHandler) {
        p.removeListener('disconnect', this.disconnectHandler);
      }
    }
    this.connectedProvider = null;
    this.connectedRdns = null;
    this.accountsHandler = null;
    this.disconnectHandler = null;
  }

  /**
   * Turn the connected address into a session:
   * nonce → ed25519 signature → verify. On success publishes `accountId`
   * and `token`; on any failure leaves both null and sets `error` so the
   * state is visible, never silent.
   */
  startSession(): void {
    const addr = this.address();
    if (addr === null) {
      this.error.set('No connected wallet');
      return;
    }
    // One handshake at a time. The connect effect can fire more than once
    // for the same address; a second nonce request would replace the first
    // on the server and the first signature would then verify against a
    // consumed nonce.
    if (this.connecting() || this.token() !== null) return;

    this.connecting.set(true);
    this.error.set(null);

    this.requestNonce(addr)
      .then((nonce) =>
        this.signMessage(buildSignInMessage(addr, nonce)).then((signature) => ({
          nonce: nonce.nonce,
          signature,
        })),
      )
      .then(({ nonce, signature }) => this.verify(addr, nonce, signature))
      .then((body) => {
        // Cross-check the server's id against the locally derived one;
        // a mismatch means we are talking to something that is not
        // the console API.
        if (body.accountId !== computeAccountId(addr)) {
          throw new Error('accountId mismatch');
        }
        this.accountId.set(body.accountId);
        this.token.set(body.token);
        this.persistSession();
      })
      .catch((err: unknown) => {
        this.failSession(err);
      })
      .finally(() => {
        this.connecting.set(false);
      });
  }

  /**
   * Drop just the connection/sign-in error card: the provider list is not
   * session state, so nothing else is cleared here.
   */
  dismissError(): void {
    this.error.set(null);
  }

  /**
   * Drop the session and everything a new one needs, so pressing connect
   * again starts a fresh sign-in without a page reload.
   */
  clearSession(): void {
    this.token.set(null);
    this.accountId.set(null);
    this.address.set(null);
    this.chainId.set(null);
    this.connecting.set(false);
    this.error.set(null);
    this.detachListeners();
    removeStoredSession();
    this.restoredRdns.set(null);
    this.restoredWallet.set(null);
  }

  /**
   * Drop just the session and its durable copy while keeping the wallet
   * connection intact. This is the reset the 401 recovery flow needs.
   */
  dropSession(): void {
    this.token.set(null);
    this.accountId.set(null);
    this.connecting.set(false);
    removeStoredSession();
    this.restoredRdns.set(null);
    this.restoredWallet.set(null);
  }

  /**
   * Ask the connected wallet to sign a prepared Solana transaction. Used by
   * every transfer path (deposit, marketplace settlement). Rejects when no
   * wallet is connected or the wallet cannot sign transactions; the caller
   * classifies the message for display.
   */
  async signSolanaTransaction(tx: Transaction): Promise<Transaction> {
    const provider = this.connectedProvider ?? this.injectedProvider;
    if (provider === null) {
      throw new Error('No connected wallet');
    }
    if (typeof provider.signTransaction !== 'function') {
      throw new Error('This wallet cannot sign transactions - use Phantom');
    }
    return provider.signTransaction(tx);
  }

  /**
   * Ask the wallet to sign `message` with its ed25519 key. The bytes are
   * signed verbatim - no EIP-191 framing - and the result is sent hex.
   */
  private signMessage(message: string): Promise<string> {
    const provider = this.connectedProvider ?? this.injectedProvider;
    if (provider === null) {
      return Promise.reject(new Error('No connected wallet'));
    }
    const bytes = new TextEncoder().encode(message);
    return provider
      .signMessage(bytes, 'utf8')
      .then((res) =>
        Array.from(res.signature, (b) => b.toString(16).padStart(2, '0')).join(''),
      );
  }

  private async requestNonce(addr: string): Promise<NonceResponse> {
    const res = await fetch(`${this.baseUrl}${NONCE_PATH}`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ address: addr }),
    });
    if (!res.ok) {
      throw await apiError(res);
    }
    return (await res.json()) as NonceResponse;
  }

  private async verify(
    addr: string,
    nonce: string,
    signature: string,
  ): Promise<VerifyResponse> {
    const res = await fetch(`${this.baseUrl}${VERIFY_PATH}`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ address: addr, nonce, signature }),
    });
    if (!res.ok) {
      if (res.status === 401) {
        throw new Error('Signature verification failed');
      }
      const err = await apiError(res);
      if (/expired/i.test(err.message)) {
        throw new Error('Sign-in nonce expired, please try again');
      }
      throw err;
    }
    return (await res.json()) as VerifyResponse;
  }

  /** Central failure path: no token, visible error. */
  private failSession(err: unknown): void {
    this.token.set(null);
    this.accountId.set(null);
    if (this.isUserCancellation(err)) {
      this.error.set('Signature rejected');
      return;
    }
    this.error.set(
      err instanceof Error ? err.message : 'Sign-in failed, please try again',
    );
  }
}
