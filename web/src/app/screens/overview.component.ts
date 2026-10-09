import { Component, computed, inject } from '@angular/core';
import { NgClass } from '@angular/common';
import { RouterLink } from '@angular/router';
import { ConsoleStore } from '../core/console-store';
import { BreakpointService } from '../core/breakpoint';
import { attestRows } from '../core/attestation-health';
import { creditText, creditText1, num, usd } from '../core/pricing';
import { localTime, relativeTime } from '../core/relative-time';
import { SkeletonComponent } from '../ui/skeleton.component';
import { ErrorBlockComponent } from '../ui/error-block.component';
import { EmptyBlockComponent } from '../ui/empty-block.component';
import { InferenceNoticeComponent } from '../ui/inference-notice.component';

@Component({
  selector: 'slf-overview',
  imports: [
    RouterLink,
    NgClass,
    SkeletonComponent,
    ErrorBlockComponent,
    EmptyBlockComponent,
    InferenceNoticeComponent,
  ],
  templateUrl: './overview.component.html',
})
export class OverviewComponent {
  private readonly breakpoints = inject(BreakpointService);
  protected readonly bp = this.breakpoints.bp;
  protected readonly store = inject(ConsoleStore);
  protected readonly recent = computed(() => this.store.usage().slice(0, 5));
  /** The attestation card's display rows; the card hides itself when empty. */
  protected readonly attestRows = computed(() => attestRows(this.store.attestation()));
  protected readonly creditText = creditText;
  protected readonly creditText1 = creditText1;
  protected readonly num = num;
  protected readonly usd = usd;
  /** Relative time for a key's last use; the only place it is computed. */
  protected readonly relTime = (ts: string | null): string => relativeTime(ts);
  /** Absolute timestamps render in the viewer's local zone, no suffix. */
  protected readonly localTime = localTime;
}
