import { isDirectPurchase } from '../core/trace';
import { Component, DestroyRef, effect, inject, computed } from '@angular/core';
import { ActivatedRoute, RouterLink } from '@angular/router';
import { toSignal } from '@angular/core/rxjs-interop';
import { map } from 'rxjs';
import { ConsoleStore } from '../core/console-store';
import { creditText, num, usd } from '../core/pricing';
import { mid } from '../core/trace';
import { localTime } from '../core/relative-time';
import { IconComponent } from '../ui/icon.component';
import { SkeletonComponent } from '../ui/skeleton.component';
import { ErrorBlockComponent } from '../ui/error-block.component';
import { EmptyBlockComponent } from '../ui/empty-block.component';

/** Circumference of the r=11 progress ring drawn around each step number. */
const RING = 2 * Math.PI * 11;

@Component({
  selector: 'slf-funding-trace',
  imports: [RouterLink, IconComponent, SkeletonComponent, ErrorBlockComponent, EmptyBlockComponent],
  templateUrl: './funding-trace.component.html',
})
export class FundingTraceComponent {
  /** A direct purchase: one deposit step, credited in the paying transaction. */
  protected readonly isDirect = computed(() => {
    const st = this.store.fundingStatus();
    return st !== null && isDirectPurchase(st);
  });
  protected readonly store = inject(ConsoleStore);
  private readonly route = inject(ActivatedRoute);

  /** The purchase named in the URL; the list page links here. */
  protected readonly purchaseId = toSignal(
    this.route.paramMap.pipe(map((m) => m.get('id'))),
    { initialValue: this.route.snapshot.paramMap.get('id') },
  );
  /** True once the history is loaded and the URL names no purchase in it. */
  protected readonly notFound = () => {
    const id = this.purchaseId();
    return id !== null && !this.store.fundingLoading() &&
      !this.store.fundingHistory().some((p) => p.purchaseId === id);
  };

  constructor() {
    // Select the routed purchase as soon as the history holds it; the
    // history may still be loading on a cold open of the URL.
    effect(() => {
      const id = this.purchaseId();
      if (id !== null && this.store.fundingHistory().some((p) => p.purchaseId === id)) {
        this.store.selectPurchase(id);
      }
    });
    // Leaving the trace hands the overview card back to the default pick.
    inject(DestroyRef).onDestroy(() => this.store.selectedPurchaseId.set(null));
  }
  protected readonly ring = RING;
  protected readonly num = num;
  protected readonly creditText = creditText;
  protected readonly usd = usd;
  /** Absolute timestamps render in the viewer's local zone, no suffix. */
  protected readonly localTime = localTime;
  /** Middle-truncation cap for wallet addresses: 18 mono chars fit the
   *  343px mobile row beside its label. */
  protected readonly mid = mid;

  protected dashOffset(progress: number): number {
    return RING * (1 - progress);
  }

  protected ticks(total: number): number[] {
    return Array.from({ length: total }, (_, i) => i);
  }
}
