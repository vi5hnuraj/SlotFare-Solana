import { Service, computed, inject, signal } from '@angular/core';
import { PreparedMarketTx, SLOTFARE_API, ApiError, SlotfareApi } from './api';
import { Listing } from './models';
import { environment } from '../../environments/environment';
import { WalletService } from './wallet';
import { isListingNotOpen, sendPreparedTransaction } from './solana';

/** Which of the write prompts (or none) the seller is on. */
export type SellStep = 'idle' | 'listing' | 'cancelling' | 'done';

/** The fixed, user-facing messages for every error class. The `rejected`
 *  key is what the templates compare against to render a dismissible
 *  notice instead of the page-level error card. */
export const SELL_ERRORS = {
  rejected: 'The request was cancelled. Nothing was sent.',
  noWallet: 'Connect a wallet first - the listing is opened from your session.',
  insufficient:
    'Not enough spendable credits for this listing. Unlock a listing or add credits first.',
  forbidden: 'Only the seller can cancel this listing.',
  gone: 'This listing is no longer open - it was bought, cancelled or superseded.',
  notFound: 'That listing no longer exists.',
  network: 'The console could not be reached. Nothing was changed.',
} as const;

/** Map any sell failure to one of the fixed, user-facing messages. Pure and
 *  deterministic on its inputs: no I/O, no state. The server's reason
 *  string wins when it is one of ours; a wallet rejection is its own class;
 *  anything else falls through to the generic network message with the
 *  underlying detail kept visible. */
export function classifySellError(err: unknown): string {
  const detail = errorText(err);
  if (/user rejected|denied|request rejected|canceled|cancelled/i.test(detail)) {
    return SELL_ERRORS.rejected;
  }
  if (/no connected wallet/i.test(detail)) return SELL_ERRORS.noWallet;
  if (/insufficient credits/i.test(detail)) return SELL_ERRORS.insufficient;
  if (/not your listing/i.test(detail)) return SELL_ERRORS.forbidden;
  if (isListingNotOpen(err)) return SELL_ERRORS.gone;
  if (/not open|already moved|no longer (open|exists)/i.test(detail)) return SELL_ERRORS.gone;
  if (/listing not found/i.test(detail)) return SELL_ERRORS.notFound;
  if (/not matured/i.test(detail)) {
    return 'This listing has not matured yet; a sale lands after the maturation window.';
  }

  if (err instanceof ApiError) {
    if (err.status === 0) return `${SELL_ERRORS.network} (${detail})`;
    // The server's reason, when the transport did not carry it.
    if (err.status === 402) return SELL_ERRORS.insufficient;
    if (err.status === 403) return SELL_ERRORS.forbidden;
    if (err.status === 404) return SELL_ERRORS.notFound;
    if (err.status === 409) return SELL_ERRORS.gone;
    if (err.status === 501) return 'The market is not available on this deployment.';
  }
  return detail !== '' ? detail : SELL_ERRORS.network;
}

/** Every readable piece of a failure as one searchable string. */
function errorText(err: unknown): string {
  if (err === null || err === undefined) return '';
  if (typeof err === 'string') return err;
  if (err instanceof Error) {
    const e = err as Error & { code?: unknown };
    return [err.message, typeof e.code === 'string' ? e.code : ''].filter(Boolean).join(' ');
  }
  return String(err);
}

/**
 * The sell flow, backed by the console API and the escrow program: `list`
 * moves the seller's credits into a program-owned escrow account, `cancel`
 * withdraws them. The daemon builds each instruction but never holds a key,
 * so the session wallet signs it here and the row is recorded from what the
 * program then holds - the escrow is the lock, not a server-side flag.
 *
 * The offline demo has no cluster, so it keeps the console's own mirror and
 * is the one deployment where the server is the writer of last resort.
 */
@Service()
export class SellService {
  private readonly api = inject(SLOTFARE_API);
  private readonly wallet = inject(WalletService);

  readonly step = signal<SellStep>('idle');
  readonly error = signal<string | null>(null);
  /** The last listing id this flow created, for the caller's toast. */
  readonly listingId = signal<string | null>(null);

  /** True while a write is in flight, so the buttons can disable themselves. */
  readonly busy = computed(() => {
    const s = this.step();
    return s === 'listing' || s === 'cancelling';
  });

  /**
   * Open a listing of `credits` priced at `priceUsd` USDC. The price is
   * scaled to the token's base units here (the wire contract takes raw
   * units), and the created row comes back for the caller to announce.
   * On failure sets `error` to the classified message and rethrows.
   */
  async list(credits: number, symbol: string, priceUsd: number): Promise<Listing> {
    this.rejectIfBusy();
    if (symbol === '') {
      const msg = SELL_ERRORS.rejected;
      this.fail(msg);
      throw new Error(msg);
    }

    this.error.set(null);
    this.step.set('listing');
    try {
      // USDC base units: the market's only token, 6 decimals, whole-cent
      // precision preserved by rounding rather than truncation.
      const price = Math.max(1, Math.round(priceUsd * 1e6));
      const row = await this.openEscrow(credits, price);
      this.listingId.set(row.id);
      this.step.set('done');
      return row;
    } catch (err) {
      const msg = classifySellError(err);
      this.fail(msg);
      throw new Error(msg);
    }
  }

  /**
   * Withdraw an open listing. The row's state follows the server after the
   * call; this returns nothing and reports failure through `error`.
   */
  async cancel(listingId: string): Promise<void> {
    this.rejectIfBusy();
    this.error.set(null);
    this.step.set('cancelling');
    try {
      await this.withdrawEscrow(listingId);
      this.step.set('done');
    } catch (err) {
      const msg = classifySellError(err);
      this.fail(msg);
      throw new Error(msg);
    }
  }

  /**
   * Open the escrow: the console builds the list instruction, the session
   * wallet signs it, and the row comes back from what the program now holds.
   * The offline demo has no cluster to sign against, so it keeps the
   * server-side mirror instead.
   */
  private async openEscrow(credits: number, price: number): Promise<Listing> {
    if (environment.useMock) {
      return this.api.createListing({ credits, price });
    }
    const prepared = await this.api.prepareMarket({ action: 'list', credits, price });
    const signature = await this.signAndSend(prepared);
    return this.api.confirmMarket({ action: 'list', nonce: prepared.nonce, signature });
  }

  /** Withdraw the escrow: the same prepare/sign/confirm round trip. */
  private async withdrawEscrow(listingId: string): Promise<Listing> {
    if (environment.useMock) {
      return this.api.cancelListing(listingId);
    }
    const id = listingId;
    const prepared = await this.api.prepareMarket({ action: 'cancel', listingId: id });
    const signature = await this.signAndSend(prepared);
    return this.api.confirmMarket({
      action: 'cancel',
      listingId: id,
      nonce: prepared.nonce,
      signature,
    });
  }

  /** Hand one prepared transaction to the wallet and send what it signs. */
  private async signAndSend(prepared: PreparedMarketTx): Promise<string> {
    const address = this.wallet.address();
    if (address === null) throw new Error(SELL_ERRORS.noWallet);
    const target = await this.api.depositTarget();
    return sendPreparedTransaction({
      rpc: target.rpc,
      preparedBase64: prepared.transaction,
      sign: (tx) => this.wallet.signSolanaTransaction(tx),
    });
  }

  /**
   * A second write for the same action must never open while one is in
   * flight: reject immediately so the server is asked exactly once.
   */
  private rejectIfBusy(): void {
    if (this.busy()) {
      throw new Error('Please wait for the pending request to finish.');
    }
  }

  /**
   * Clear everything the flow's error card shows: the message, the prompt
   * step and the last id. Dismiss, a new action, navigation back and
   * disconnect all go through here, so no card survives a flow reset.
   */
  dismissError(): void {
    this.error.set(null);
    this.step.set('idle');
    this.listingId.set(null);
  }

  /** Publish the classified message and drop back to idle. */
  private fail(message: string): void {
    this.error.set(message);
    this.step.set('idle');
  }
}
