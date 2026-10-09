import { Service, computed, inject, signal } from '@angular/core';
import { ApiError, PreparedMarketTx, SLOTFARE_API, SlotfareApi } from './api';
import { MarketListing } from './models';
import { environment } from '../../environments/environment';
import { WalletService } from './wallet';
import { TransferError, isListingNotOpen, isSelfBuy, sendPreparedTransaction } from './solana';

/** Which phase of the buy flow the service is on. */
export type BuyStep = 'idle' | 'paying' | 'done';

/** The fixed, user-facing messages for every error class. `rejected` is
 *  what the templates compare against to render a dismissible notice
 *  instead of the page-level error card; `boughtByAnother` is surfaced
 *  through the `listingGone` flag as a plain notice. */
export const BUY_ERRORS = {
  rejected: 'You cancelled the request in your wallet. Nothing was sent.',
  noWallet: 'Connect a wallet first - the payment is sent from your wallet.',
  notMature:
    'This listing has not matured yet; it can be bought once its maturation window has passed.',
  boughtByAnother:
    'Someone else bought this listing first. Nothing was paid and the listing is gone.',
  noSeller:
    'This listing carries no seller wallet. Reload the market and try again.',
  insufficient: 'Your wallet does not hold enough USDC for this price.',
  ownListing: 'This listing is your own - the escrow will not sell it back to you.',
  network:
    'The console could not be reached. Check your wallet activity before retrying.',
} as const;

/**
 * Map any buy failure to one of the fixed, user-facing messages. Pure and
 * deterministic on its inputs: no I/O, no state. Order: wallet rejection,
 * insufficient USDC, then the server's reasons, then the transport classes.
 */
export function classifyBuyError(err: unknown): string {
  const detail = errorText(err);
  if (err instanceof TransferError && err.cancelled) return BUY_ERRORS.rejected;
  if (/user rejected|denied|request rejected|canceled|cancelled/i.test(detail)) {
    return BUY_ERRORS.rejected;
  }
  // The program's own rejections arrive on the wallet's transaction, so they
  // are read before the transfer classes: a listing the escrow refuses to
  // sell is not an insufficient balance.
  if (isListingNotOpen(err)) return BUY_ERRORS.boughtByAnother;
  if (isSelfBuy(err)) return BUY_ERRORS.ownListing;
  if (
    err instanceof TransferError ||
    /not enough usdc|insufficient (funds|tokens|lamports)|attempt to debit/i.test(detail)
  ) {
    return BUY_ERRORS.insufficient;
  }
  if (/no connected wallet/i.test(detail)) return BUY_ERRORS.noWallet;
  if (/no seller wallet/i.test(detail)) return BUY_ERRORS.noSeller;
  if (/not[\s-]?matured/i.test(detail)) return BUY_ERRORS.notMature;
  if (/not open|already moved|no longer open/i.test(detail)) return BUY_ERRORS.boughtByAnother;

  if (err instanceof ApiError) {
    if (err.status === 0) return `${BUY_ERRORS.network} (${detail})`;
    if (err.status === 409) return BUY_ERRORS.boughtByAnother;
    if (err.status === 404) return BUY_ERRORS.boughtByAnother;
    if (err.status === 501) return 'The market is not available on this deployment.';
  }
  return detail !== '' ? detail : BUY_ERRORS.network;
}

/** Every readable piece of a failure as one searchable string. */
function errorText(err: unknown): string {
  if (err === null || err === undefined) return '';
  if (typeof err === 'string') return err;
  if (err instanceof Error) return err.message;
  return String(err);
}

/**
 * The buy flow: the console builds the escrow's Buy instruction - an atomic
 * payment to the seller and a release of the escrowed credits - the wallet
 * signs it, and the row is recorded from what the program then holds. The
 * sale is one transaction, so there is no window in which a buyer has paid
 * without receiving the credits, or the reverse.
 *
 * The offline demo has no cluster, so it keeps the console's mirror and a
 * stand-in signature.
 */
@Service()
export class BuyService {
  private readonly api = inject(SLOTFARE_API);
  private readonly wallet = inject(WalletService);

  readonly step = signal<BuyStep>('idle');
  readonly error = signal<string | null>(null);
  /** True when the last failure was a lost race (or a moved row): the
   *  template shows a plain notice and refreshes, never an error card. */
  readonly listingGone = signal(false);

  /** True while the flow is in flight, so the buttons can disable. */
  readonly busy = computed(() => this.step() === 'paying');

  /**
   * Buy the whole listing: transfer its price from this wallet to the
   * seller, then settle against the signature. Returns the transfer
   * signature on success. On failure sets `error` to the classified
   * message (and `listingGone` when the row moved), then rethrows.
   */
  async buy(l: MarketListing): Promise<string> {
    this.rejectIfBusy();
    this.error.set(null);
    this.listingGone.set(false);
    this.step.set('paying');
    try {
      const signature = environment.useMock ? await this.payOffline(l) : await this.payOnchain(l);
      this.step.set('done');
      return signature;
    } catch (err) {
      const moved =
        (err instanceof ApiError && err.status === 409 && !/not matured/i.test(errorText(err))) ||
        isListingNotOpen(err);
      if (moved) this.listingGone.set(true);
      const msg = classifyBuyError(err);
      this.error.set(msg);
      this.step.set('idle');
      throw new Error(msg);
    }
  }

  /**
   * The offline demo: settle against a stand-in signature, exactly like the
   * mock's other write fixtures.
   */
  private async payOffline(l: MarketListing): Promise<string> {
    const signature = `MockBuySig${l.id}${Date.now()}`;
    await this.api.payListing(l.id, signature);
    return signature;
  }

  /**
   * The live market: ask the console for a buyer-signed transaction, sign it
   * with the session wallet, send it, and hand the signature back so the
   * console can record the sale it re-reads from the program.
   */
  private async payOnchain(l: MarketListing): Promise<string> {
    const address = this.wallet.address();
    if (address === null) throw new Error(BUY_ERRORS.noWallet);
    // A row with no seller wallet has no escrow to take; the market view
    // always carries one, and a row that does not is a broken listing.
    if (typeof l.seller !== 'string' || l.seller === '') {
      throw new Error(BUY_ERRORS.noSeller);
    }
    const id = l.id;
    const prepared: PreparedMarketTx = await this.api.prepareMarket({ action: 'buy', listingId: id });
    const target = await this.api.depositTarget();
    const signature = await sendPreparedTransaction({
      rpc: target.rpc,
      preparedBase64: prepared.transaction,
      sign: (tx) => this.wallet.signSolanaTransaction(tx),
    });
    await this.api.confirmMarket({
      action: 'buy',
      listingId: id,
      nonce: prepared.nonce,
      signature,
    });
    return signature;
  }

  /**
   * A second prompt must never open while one is in flight: reject
   * immediately so the wallet is asked exactly once.
   */
  private rejectIfBusy(): void {
    if (this.busy()) {
      throw new Error('Please wait for the pending payment to finish.');
    }
  }

  /** Clear everything the flow shows: message, step and the lost-race
   *  flag. Dismiss, a new action and navigation all go through here. */
  dismissError(): void {
    this.error.set(null);
    this.step.set('idle');
    this.listingGone.set(false);
  }
}
