import { Component, computed, inject } from '@angular/core';
import { RouterLink } from '@angular/router';
import {
  ConsoleStore,
  TransactionFilter,
} from '../core/console-store';
import { creditText } from '../core/pricing';
import { localTime } from '../core/relative-time';
import { IconComponent } from '../ui/icon.component';
import { SkeletonComponent } from '../ui/skeleton.component';
import { ErrorBlockComponent } from '../ui/error-block.component';
import { EmptyBlockComponent } from '../ui/empty-block.component';

/** The account's money in one list: deposits proven/direct, market buys and
 *  sells. A row opens its trace, the sell screen, or the explorer link. */
@Component({
  selector: 'slf-purchases',
  imports: [RouterLink, IconComponent, SkeletonComponent, ErrorBlockComponent, EmptyBlockComponent],
  templateUrl: './purchases.component.html',
})
export class PurchasesComponent {
  protected readonly store = inject(ConsoleStore);
  protected readonly creditText = creditText;
  /** Absolute timestamps render in the viewer's local zone, no suffix. */
  protected readonly localTime = localTime;

  /** The three filters, in display order. */
  protected readonly filters: { key: TransactionFilter; label: string }[] = [
    { key: 'all', label: 'All' },
    { key: 'buying', label: 'Buying' },
    { key: 'selling', label: 'Selling' },
  ];

  constructor() {
    // The shared 15 s loop (owned by the session) keeps every live figure
    // fresh; this screen only subscribes to the store's signals.
  }

  /** Switch the client-side filter; no request is made. */
  protected pickFilter(key: TransactionFilter): void {
    this.store.setTransactionFilter(key);
  }

  /** True when this filter is the active one. */
  protected isActive(key: TransactionFilter): boolean {
    return this.store.transactionFilter() === key;
  }

  /** The muted type label above the status pill. */
  protected kindLabel(kind: string): string {
    switch (kind) {
      case 'deposit':
        return 'Deposit';
      case 'buy':
        return 'Buy';
      default:
        return 'Sell';
    }
  }

  /** True while the Transactions list can load more older rows: the server
   *  reports a live cursor and no load-more is in flight. */
  protected readonly hasMore = computed(
    () => this.store.transactionsHasMore() && !this.store.loadingMoreTransactions(),
  );

  /** Load the next page of older rows, appending in place. */
  protected loadMore(): void {
    void this.store.loadMoreTransactions();
  }
}
