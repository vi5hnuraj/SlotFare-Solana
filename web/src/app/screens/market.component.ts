import { Component, computed, effect, inject, signal } from '@angular/core';
import { Router } from '@angular/router';
import { ConsoleStore } from '../core/console-store';
import { BuyService, BUY_ERRORS } from '../core/buy';
import { SellService, SELL_ERRORS } from '../core/sell';
import { MarketListing } from '../core/models';
import { creditText, dec2, num } from '../core/pricing';
import { listingPrice } from '../core/listing-price';
import { maturityHint } from '../core/relative-time';
import { IconComponent } from '../ui/icon.component';
import { SkeletonComponent } from '../ui/skeleton.component';
import { ErrorBlockComponent } from '../ui/error-block.component';
import { EmptyBlockComponent } from '../ui/empty-block.component';
import { ToastService } from '../core/toast';
import { SellComponent } from './sell.component';

@Component({
  selector: 'slf-market',
  imports: [IconComponent, SkeletonComponent, ErrorBlockComponent, EmptyBlockComponent, SellComponent],
  templateUrl: './market.component.html',
})
export class MarketComponent {
  private readonly toast = inject(ToastService);
  private readonly router = inject(Router);
  protected readonly store = inject(ConsoleStore);
  protected readonly buy = inject(BuyService);
  protected readonly sell = inject(SellService);
  protected readonly num = num;
  protected readonly creditText = creditText;
  protected readonly dec2 = dec2;
  /** A stable "now" for the maturity hint; the fixture drives tests, not a
   *  live clock read in the template. */
  private readonly now = Date.now();
  protected readonly maturityHint = (l: MarketListing): string =>
    maturityHint(l.maturesAt, this.now);

  /** The scaled price of one market row, or the raw figure with its marker. */
  protected priceText(l: MarketListing): string {
    return listingPrice(l).text;
  }
  /** The pay token's symbol, else the shortened address, else the
   *  placeholder. It is never a bare truncated hex fragment. */
  protected priceLabel(l: MarketListing): string {
    return listingPrice(l).label;
  }

  /** The buy service's lost-race flag and error, exposed for the template. */
  protected readonly buyListingGone = computed(() => this.buy.listingGone());
  protected readonly buyError = computed(() => this.buy.error());

  /** The sell service's busy flag and error, exposed for the template. */
  protected readonly sellBusy = computed(() => this.sell.busy());
  protected readonly cancelError = computed(() => this.sell.error());
  /** The fixed rejection messages; the template compares against them to
   *  render a dismissible notice instead of the page-level error card. */
  protected readonly buyRejected = BUY_ERRORS.rejected;
  protected readonly cancelRejected = SELL_ERRORS.rejected;

  /** The inline panel that hosts the sell flow in place; opening it keeps
   *  the user on the market, the standalone /sell route is untouched. */
  protected readonly sellPanelOpen = signal(false);
  /** The pending listing seen at entry, so a listing that predates the
   *  visit does not retrigger a market load on entry. */
  private lastPending: string | null = this.store.pendingListingId();

  constructor() {
    // Entering the screen must start clean: both flows' error cards are
    // cleared on entry, never carried over from a previous visit.
    this.buy.dismissError();
    this.sell.dismissError();
    void this.store.loadMarket();
    // Hosting the sell flow announces listings through the shared store:
    // a new pendingListingId means one was just sent from this page, so
    // refresh the market list and the new row shows up in place.
    effect(() => {
      const pending = this.store.pendingListingId();
      if (pending !== null && pending !== this.lastPending) {
        this.lastPending = pending;
        void this.store.loadMarket();
      }
    });
  }

  /**
   * Open the sell panel. Opening constructs a fresh SellComponent, whose
   * own entry-time sell.dismissError() clears any cancel rejection notice
   * showing at that moment - the app's existing "a new action clears the
   * card before the attempt" convention; no extra handling.
   */
  protected openSellPanel(): void {
    this.sellPanelOpen.set(true);
  }

  /** Close the panel: the component is destroyed with its form state, the
   *  market list is untouched and no navigation happens. */
  protected closeSellPanel(): void {
    this.sellPanelOpen.set(false);
  }

  /** A listing is buyable only when it is not the customer's own (the
   *  server's `mine` boolean), is matured per the server's boolean, and no
   *  payment is in flight. Whole listings only: the server settles whole
   *  listings, so there is no quantity field. */
  protected canBuy(l: MarketListing): boolean {
    return !l.mine && l.matured && !this.buy.busy();
  }

  /**
   * Buy the whole listing: the wallet sends the seller the price in USDC,
   * and the call returns only once the server has verified that transfer
   * and moved the credits. We then announce the overview's buy card and
   * land there; on a lost race we refresh the list and show a plain
   * notice; on any other failure we stay and surface the classified error.
   */
  protected async doBuy(l: MarketListing): Promise<void> {
    if (!this.canBuy(l)) return;
    try {
      const signature = await this.buy.buy(l);
      const lp = listingPrice(l);
      this.store.announceBuy(signature, l.credits, `${lp.text} ${lp.label}`);
      this.toast.say(`Purchased ${creditText(l.credits)} credits`);
      void this.router.navigate(['/overview']);
    } catch {
      if (this.buy.listingGone()) {
        // Lost the race to another buyer: a normal outcome. Refresh the list
        // so the row disappears and drop the error into a plain notice.
        void this.store.loadMarket();
        return;
      }
      // buy.error() now holds the classified message; the panel renders it.
    }
  }

  /**
   * Cancel one's own open listing through the sell screen's flow: the
   * console withdraws it under the session, then the list refreshes. On
   * failure sell.error() holds the classified message and the panel
   * renders it in place.
   */
  protected async doCancel(l: MarketListing): Promise<void> {
    if (!l.mine || this.sell.busy()) return;
    try {
      await this.store.cancelListing(l.id);
      this.toast.say('Cancel sent for listing ' + l.id);
      void this.store.loadMarket();
    } catch {
      // cancelListing rethrows after classifying; sell.error() shows in place.
    }
  }
}
