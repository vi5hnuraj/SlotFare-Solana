import { Service, inject, signal } from '@angular/core';
import { ApiError, DepositTarget, SLOTFARE_API, SlotfareApi } from './api';
import { environment } from '../../environments/environment';
import { WalletService } from './wallet';
import { TransferError, sendTokenTransfer } from './solana';

/** The fixed, user-facing messages for every deposit error class.
 *  `rejected` is what the template compares against to render a
 *  dismissible notice instead of the page-level error card. */
export const DEPOSIT_ERRORS = {
  rejected: 'You cancelled the request in your wallet. Nothing was sent.',
  noWallet: 'Connect a wallet first - the payment is sent from your wallet.',
  noTarget:
    'Deposits are not configured on this deployment (the deposit target is unavailable).',
  insufficient: 'Your wallet does not hold enough USDC for this amount.',
  network:
    'The console could not be reached. Check your wallet activity before retrying.',
} as const;

/** Scale whole USDC to base units (6 decimals), rounded to whole cents. */
export function usdcToBase(amountUsd: number): bigint {
  return BigInt(Math.max(0, Math.round(amountUsd * 1e6)));
}

/** Map any deposit failure to one of the fixed, user-facing messages. */
export function classifyDepositError(err: unknown): string {
  const detail = err instanceof Error ? err.message : String(err ?? '');
  if (err instanceof TransferError && err.cancelled) return DEPOSIT_ERRORS.rejected;
  if (/user rejected|denied|request rejected|canceled|cancelled/i.test(detail)) {
    return DEPOSIT_ERRORS.rejected;
  }
  if (
    err instanceof TransferError ||
    /not enough usdc|insufficient (funds|tokens|lamports)|attempt to debit/i.test(detail)
  ) {
    return DEPOSIT_ERRORS.insufficient;
  }
  if (/no connected wallet/i.test(detail)) return DEPOSIT_ERRORS.noWallet;
  if (err instanceof ApiError && (err.status === 501 || /not available/i.test(detail))) {
    return DEPOSIT_ERRORS.noTarget;
  }
  if (err instanceof ApiError && err.status === 0) {
    return `${DEPOSIT_ERRORS.network} (${detail})`;
  }
  return detail !== '' ? detail : DEPOSIT_ERRORS.network;
}

/**
 * The deposit flow: send USDC from the connected wallet to the treasury
 * token account the console serves, then hand the signature back. The
 * backend's watcher sees the same transfer on chain and mints the credits;
 * the caller announces the purchase and opens its trace.
 */
@Service()
export class DepositService {
  private readonly api = inject(SLOTFARE_API);
  private readonly wallet = inject(WalletService);

  readonly busy = signal(false);
  readonly error = signal<string | null>(null);

  /** The deposit target, fetched once per screen entry so the UI can show
   *  the cluster and destination before anyone clicks Pay. Null until it
   *  loads; the pay gate treats null as "not configured". */
  readonly target = signal<DepositTarget | null>(null);

  /** Load the deposit target. A failed read leaves the signal null; the
   *  pay path then fails with `noTarget` instead of guessing a chain. */
  async loadTarget(): Promise<void> {
    if (this.target() !== null) return;
    try {
      this.target.set(await this.api.depositTarget());
    } catch {
      // Null stays null; the screen renders the unconfigured state.
    }
  }

  /**
   * Send `amountUsd` in USDC to the treasury and return the transaction
   * signature. In the offline demo there is no cluster, so a stand-in
   * signature is returned instead. On failure sets `error` to the
   * classified message and rethrows.
   */
  async deposit(amountUsd: number): Promise<string> {
    if (this.busy()) {
      throw new Error('Please wait for the pending payment to finish.');
    }
    this.error.set(null);
    this.busy.set(true);
    try {
      if (environment.useMock) {
        return `MockDepositSig${Date.now()}`;
      }
      let target = this.target();
      if (target === null) {
        target = await this.api.depositTarget();
        this.target.set(target);
      }
      if (target === null) throw new Error(DEPOSIT_ERRORS.noTarget);
      const address = this.wallet.address();
      if (address === null) throw new Error(DEPOSIT_ERRORS.noWallet);
      return await sendTokenTransfer({
        target: { rpc: target.rpc, mint: target.mint, decimals: target.decimals },
        payerAddress: address,
        amountBase: usdcToBase(amountUsd),
        destination: { kind: 'account', address: target.treasury },
        sign: (tx) => this.wallet.signSolanaTransaction(tx),
      });
    } catch (err) {
      const msg = classifyDepositError(err);
      this.error.set(msg);
      throw new Error(msg);
    } finally {
      this.busy.set(false);
    }
  }

  /** Clear the error card: dismiss, a new action and navigation all go
   *  through here, so no card survives a flow reset. */
  dismissError(): void {
    this.error.set(null);
  }
}
