import { Component, computed, inject, signal } from '@angular/core';
import { Router } from '@angular/router';
import { ConsoleStore } from '../core/console-store';
import { BreakpointService } from '../core/breakpoint';
import { SellService, SELL_ERRORS } from '../core/sell';
import { creditText, dec2, usd } from '../core/pricing';
import { listingPrice } from '../core/listing-price';
import { Listing } from '../core/models';
import { IconComponent } from '../ui/icon.component';
import { SkeletonComponent } from '../ui/skeleton.component';
import { ErrorBlockComponent } from '../ui/error-block.component';
import { EmptyBlockComponent } from '../ui/empty-block.component';
import { ToastService } from '../core/toast';

@Component({
  selector: 'slf-sell',
  imports: [
    IconComponent,
    SkeletonComponent,
    ErrorBlockComponent,
    EmptyBlockComponent,
  ],
  templateUrl: './sell.component.html',
})
export class SellComponent {
  private readonly toast = inject(ToastService);
  private readonly breakpoints = inject(BreakpointService);
  protected readonly bp = this.breakpoints.bp;
  protected readonly store = inject(ConsoleStore);
  protected readonly sell = inject(SellService);
  protected readonly creditText = creditText;
  protected readonly dec2 = dec2;
  protected readonly usd = usd;
  /** The accepted pay tokens: USDC is the market's one token (the deposit
   *  target serves the mint at runtime; the price row carries its
   *  decimals). */
  protected readonly tokens = [{ symbol: 'USDC' }];
  /** The fixed rejection message; the template compares against it to render
   *  a dismissible notice instead of the page-level error card. */
  protected readonly rejected = SELL_ERRORS.rejected;

  constructor() {
    // Leaving and coming back to the screen must start clean: the flow's
    // error card is cleared on entry, never carried over.
    this.sell.dismissError();
  }

  // ---- form state ------------------------------------------------------
  /** Whole credits to list; the input writes it. */
  protected readonly creditsInput = signal('');
  /** Whole-token price in the chosen pay token; the input writes it. */
  protected readonly priceInput = signal('');
  /** The chosen pay token symbol; the one accepted token to start with. */
  protected readonly token = signal('USDC');

  protected readonly credits = computed(() => {
    const v = Number(this.creditsInput());
    return Number.isFinite(v) ? Math.trunc(v) : 0;
  });
  protected readonly price = computed(() => {
    const v = Number(this.priceInput());
    return Number.isFinite(v) ? v : 0;
  });
  /** The spendable balance after this listing, floored at zero. */
  protected readonly spendableAfter = computed(() =>
    Math.max(0, this.store.balance() - this.credits()),
  );

  /**
   * What the button refuses before the wallet opens: zero credits, a zero
   * price, or more credits than the spendable balance. Locked credits are
   * not spendable, so they are never listed twice.
   */
  protected readonly canList = computed(
    () =>
      this.credits() > 0 &&
      this.price() > 0 &&
      this.credits() <= this.store.balance(),
  );
  protected readonly refusalReason = computed(() => {
    if (this.credits() <= 0) return 'Enter the amount of credits to list.';
    if (this.price() <= 0) return 'Enter a price above zero.';
    if (this.credits() > this.store.balance()) {
      return `Only ${creditText(this.store.balance())} credits are spendable; ${creditText(this.store.locked())} are locked by open listings.`;
    }
    return '';
  });

  protected onCredits(event: Event): void {
    this.creditsInput.set((event.target as HTMLInputElement).value);
  }

  protected onPrice(event: Event): void {
    this.priceInput.set((event.target as HTMLInputElement).value);
  }

  protected pickToken(symbol: string): void {
    this.token.set(symbol);
  }

  /**
   * Open the listing through the session. Refusals happen before the
   * request; on success the returned row shows up locked immediately and
   * the poll picks up the server's version; on failure we stay on the
   * screen and the review panel shows the classified error.
   */
  protected async doList(): Promise<void> {
    if (!this.canList() || this.sell.busy()) return;
    try {
      const row = await this.sell.list(this.credits(), this.token(), this.price());
      this.store.announceListing(row);
      this.creditsInput.set('');
      this.priceInput.set('');
      this.toast.say(`Listing created: #${row.id}`);
    } catch {
      // sell.error() now holds the message; the panel renders it.
    }
  }

  /** Cancel an open listing; the row's state follows the server after.
   *  A second click while a prompt is in flight is a no-op: the wallet is
   *  asked exactly once per action. */
  protected async cancel(l: { id: string }): Promise<void> {
    if (this.sell.busy()) return;
    try {
      await this.store.cancelListing(l.id);
      this.toast.say(`Cancelled listing #${l.id}`);
    } catch {
      // store.listingsError() now holds the message; the panel renders it.
    }
  }

  /** The My-listings price cell: the raw wire amount scaled by the pay
   *  token's decimals, next to the symbol (or shortened address). */
  protected listingPriceCell(l: Listing): string {
    const lp = listingPrice(l);
    return `${lp.text} ${lp.label}`;
  }
}
