import { Component, computed, inject } from '@angular/core';
import { ActivatedRoute, RouterLink } from '@angular/router';
import { toSignal } from '@angular/core/rxjs-interop';
import { map } from 'rxjs';
import { ConsoleStore } from '../core/console-store';
import { creditText, num } from '../core/pricing';
import { listingPrice } from '../core/listing-price';
import { listingStatusLabel } from '../core/role-label';
import { solTxUrl, short } from '../core/trace';
import { localTime } from '../core/relative-time';
import { Listing } from '../core/models';
import { IconComponent } from '../ui/icon.component';
import { SkeletonComponent } from '../ui/skeleton.component';
import { EmptyBlockComponent } from '../ui/empty-block.component';

/** One label/value fact the detail view renders. */
interface FactRow {
  label: string;
  value: string;
}

/** The one transaction a finished trade settles with: its hash, block and
 *  when it landed, plus the explorer link. */
interface TxFact {
  title: string;
  hash: string;
  block: number | null;
  at: string;
  href: string;
}

@Component({
  selector: 'slf-trade-detail',
  imports: [RouterLink, IconComponent, SkeletonComponent, EmptyBlockComponent],
  templateUrl: './trade-detail.component.html',
})
export class TradeDetailComponent {
  protected readonly store = inject(ConsoleStore);
  private readonly route = inject(ActivatedRoute);

  /** The listing named in the URL; the transactions table links here. */
  protected readonly tradeId = toSignal(
    this.route.paramMap.pipe(map((m) => m.get('id'))),
    { initialValue: this.route.snapshot.paramMap.get('id') },
  );

  /** The routed listing row, or null while the id is unset or unknown. */
  protected readonly listing = computed<Listing | null>(() => {
    const id = this.tradeId();
    if (id === null) return null;
    return this.store.listings().find((l) => l.id === id) ?? null;
  });

  /** True once the listings are loaded and the URL names no row in them. */
  protected readonly notFound = () => {
    const id = this.tradeId();
    return (
      id !== null &&
      !this.store.listingsLoading() &&
      !this.store.listings().some((l) => l.id === id)
    );
  };

  constructor() {
    // A fresh read on cold open of the URL: the row may not be loaded yet.
    void this.store.loadListings();
  }

  protected readonly num = num;
  protected readonly short = short;
  /** Absolute timestamps render in the viewer's local zone, no suffix. */
  protected readonly localTime = localTime;

  protected price(l: Listing): string {
    const lp = listingPrice(l);
    return `${lp.text} ${lp.label}`;
  }

  protected credits(l: Listing): string {
    return creditText(l.credits);
  }

  /** The maturation deadline as a timestamp cell; dash when absent. */
  protected maturityDeadline(l: Listing): string {
    if (typeof l.maturesAt !== 'number' || l.maturesAt <= 0) return '-';
    return localTime(new Date(l.maturesAt * 1000).toISOString());
  }

  protected blockText(b: number | undefined): string {
    return typeof b === 'number' && b > 0 ? num(b) : '-';
  }

  /** The listing transaction fact. */
  protected listingTx(l: Listing): TxFact {
    return {
      title: 'Listing transaction',
      hash: l.listedTx,
      block: l.listedBlock ?? null,
      at: l.createdAt,
      href: solTxUrl(l.listedTx),
    };
  }

  /** The sale transaction fact; null until the wire carries one. */
  protected saleTx(l: Listing): TxFact | null {
    if (!l.soldTx) return null;
    return {
      title: 'Sale transaction',
      hash: l.soldTx,
      block: l.soldBlock ?? null,
      at: l.soldAt ?? '',
      href: solTxUrl(l.soldTx),
    };
  }

  /** The cancel transaction fact; null until the wire carries one. */
  protected cancelTx(l: Listing): TxFact | null {
    if (!l.cancelledTx) return null;
    return {
      title: 'Cancel transaction',
      hash: l.cancelledTx,
      block: l.cancelledBlock ?? null,
      at: l.cancelledAt ?? '',
      href: solTxUrl(l.cancelledTx),
    };
  }

  /** The outcome from the caller's role: what they ended up with. */
  protected outcome(l: Listing): string {
    if (l.state === 'sold') {
      return l.role === 'buying'
        ? `Credits bought: +${creditText(l.credits)}`
        : `Proceeds received: ${this.price(l)}`;
    }
    // Cancelled.
    return l.role === 'selling'
      ? `Credits released: ${creditText(l.credits)} back to your balance`
      : 'No credits moved';
  }

  /** The pill label, from the viewer's side of the trade. */
  protected statusLabel(l: Listing): string {
    return listingStatusLabel(l.role, l.state, l.matured);
  }

  /** Sale for the seller, Purchase for the buyer. */
  protected tradeWord(l: Listing): string {
    return l.role === 'buying' ? 'Purchase' : 'Sale';
  }

  /** The settlement timestamp row's label, from the viewer's side. */
  protected completedAtLabel(l: Listing): string {
    return l.role === 'buying' ? 'Bought at' : 'Sold at';
  }
}
