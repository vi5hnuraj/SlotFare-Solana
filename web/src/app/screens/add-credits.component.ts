import { Component, computed, inject } from '@angular/core';
import { Router } from '@angular/router';
import { ConsoleStore } from '../core/console-store';
import { BreakpointService } from '../core/breakpoint';
import { DepositService, DEPOSIT_ERRORS } from '../core/deposit';
import { creditText, creditText1 } from '../core/pricing';
import { shortAddress } from '../core/listing-price';
import { PRESET_AMOUNTS } from '../core/mock-data';
import { IconComponent } from '../ui/icon.component';
import { SkeletonComponent } from '../ui/skeleton.component';
import { ErrorBlockComponent } from '../ui/error-block.component';
import { EmptyBlockComponent } from '../ui/empty-block.component';
import { ToastService } from '../core/toast';

@Component({
  selector: 'slf-add-credits',
  imports: [
    IconComponent,
    SkeletonComponent,
    ErrorBlockComponent,
    EmptyBlockComponent,
  ],
  templateUrl: './add-credits.component.html',
})
export class AddCreditsComponent {
  private readonly router = inject(Router);
  private readonly toast = inject(ToastService);
  private readonly breakpoints = inject(BreakpointService);
  protected readonly bp = this.breakpoints.bp;
  protected readonly store = inject(ConsoleStore);
  protected readonly deposit = inject(DepositService);
  protected readonly presets = PRESET_AMOUNTS;
  protected readonly creditText = creditText;
  protected readonly creditText1 = creditText1;
  protected readonly shortAddress = shortAddress;
  /** The fixed rejection message; the template compares against it to render
   *  a dismissible notice instead of the page-level error card. */
  protected readonly rejected = DEPOSIT_ERRORS.rejected;
  /** The fixed not-configured message, reused as the disabled reason. */
  protected readonly noTarget = DEPOSIT_ERRORS.noTarget;

  constructor() {
    // Entering the screen must start clean: the flow's error card is
    // cleared on entry, never carried over from a previous visit, and the
    // deposit target is fetched once so the card can name the cluster and
    // destination before anyone clicks Pay.
    this.deposit.dismissError();
    void this.deposit.loadTarget();
  }

  protected onCustomAmount(event: Event): void {
    const value = parseFloat((event.target as HTMLInputElement).value);
    this.store.setAmount(Number.isNaN(value) ? 0 : value);
  }

  /** Preset click: set the amount; the rate is fixed, so no re-quote. */
  protected onPreset(p: number): void {
    this.store.setAmount(p);
  }

  /** The deposit target once loaded; null while it loads or when the
   *  deployment has no Solana surface. */
  protected readonly target = this.deposit.target;

  /** Balance after the deposit lands: current plus the previewed mint. */
  protected readonly newBalance = computed(
    () => this.store.balance() + this.store.creditsToBuy(),
  );

  protected readonly busy = computed(() => this.deposit.busy());
  protected readonly error = computed(() => this.deposit.error());

  /** Explorer address page for the treasury token account, or '' when the
   *  target is unknown - the link simply does not render. */
  protected readonly treasuryHref = computed(() => {
    const t = this.target();
    if (t === null || t.explorer === '') return '';
    return `${t.explorer.replace(/\/+$/, '')}/address/${t.treasury}`;
  });

  /**
   * What the button refuses before the wallet opens: an amount outside the
   * bounds, a payment already in flight, or a deployment without a
   * configured deposit target.
   */
  protected readonly payEnabled = computed(
    () => this.store.amountValid() && !this.busy() && this.target() !== null,
  );

  protected readonly payLabel = computed(() =>
    this.busy()
      ? 'Confirm in your wallet...'
      : `Pay ${this.store.amount().toFixed(2)} USDC`,
  );

  protected readonly payDisabledReason = computed(() => {
    if (this.target() === null) return this.noTarget;
    if (!this.store.amountValid()) {
      return 'Enter an amount between $5 and $50,000.';
    }
    return '';
  });

  /**
   * Send the USDC transfer from the connected wallet to the treasury, then
   * announce the purchase the moment the signature returns and open its
   * trace: the backend's watcher records the same transfer and the trace
   * runs from pending to credited on its own. On failure we stay on the
   * screen and the review panel shows the classified error.
   */
  protected async pay(): Promise<void> {
    if (!this.payEnabled()) return;
    try {
      const signature = await this.deposit.deposit(this.store.amount());
      this.store.announcePurchase(
        signature,
        this.store.amount(),
        'USDC',
        0,
        'Solana',
      );
      this.toast.say(
        `Deposit sent: ${signature.slice(0, 10)}...${signature.slice(-6)}`,
      );
      void this.router.navigate(['/funding', signature]);
    } catch {
      // deposit.error() now holds the message; the panel renders it.
    }
  }

}
