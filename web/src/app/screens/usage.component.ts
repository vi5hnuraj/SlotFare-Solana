import { Component, computed, inject, signal } from '@angular/core';
import { ConsoleStore, ProofCheck, UsageStatusFilter } from '../core/console-store';
import { DAILY_SPEND, DAILY_REQUESTS, DAY_ZERO } from '../core/mock-data';
import { bucketDaily, dayZeroFor, periodStats, DAY_MS } from '../core/burn';
import { creditText, num, usd } from '../core/pricing';
import { humanDuration, localTime } from '../core/relative-time';
import { solTxUrl, short } from '../core/trace';
import { UsageRow } from '../core/models';
import { environment } from '../../environments/environment';
import { IconComponent } from '../ui/icon.component';
import { CopyButtonComponent } from '../ui/copy-button.component';
import { SpendChartComponent } from '../ui/spend-chart.component';
import { SkeletonComponent } from '../ui/skeleton.component';
import { ErrorBlockComponent } from '../ui/error-block.component';
import { EmptyBlockComponent } from '../ui/empty-block.component';
import { SearchSelectComponent } from '../ui/search-select.component';

/** Map the three range labels to the day counts they fetch. */
const RANGE_DAYS: Record<string, number> = {
  '7 d': 7,
  '30 d': 30,
  '90 d': 90,
};

/** A local calendar date (YYYY-MM-DD) as a native date input reads it. */
function dateInputValue(d: Date): string {
  const m = `${d.getMonth() + 1}`.padStart(2, '0');
  const day = `${d.getDate()}`.padStart(2, '0');
  return `${d.getFullYear()}-${m}-${day}`;
}

/** unix seconds of the start of a local calendar day; '' means unset (0). */
function fromStartSec(value: string): number {
  if (value === '') return 0;
  const t = Date.parse(`${value}T00:00:00`);
  return Number.isNaN(t) ? 0 : Math.floor(t / 1000);
}

/** unix seconds of the end of a local calendar day; '' means unset (0). */
function toEndSec(value: string): number {
  if (value === '') return 0;
  const t = Date.parse(`${value}T23:59:59.999`);
  return Number.isNaN(t) ? 0 : Math.floor(t / 1000);
}

/** The local calendar date a unix-seconds bound was picked on, for display. */
function secToDateInput(sec: number): string {
  if (sec <= 0) return '';
  return dateInputValue(new Date(sec * 1000));
}

@Component({
  selector: 'slf-usage',
  imports: [IconComponent, CopyButtonComponent, SpendChartComponent, SkeletonComponent, ErrorBlockComponent, EmptyBlockComponent, SearchSelectComponent],
  templateUrl: './usage.component.html',
})
export class UsageComponent {
  protected readonly store = inject(ConsoleStore);
  protected readonly creditText = creditText;
  protected readonly num = num;
  protected readonly usd = usd;
  /** The commit transaction opens in the Solana explorer; usage roots
   *  settle as memo transactions on the Slotfare cluster. */
  protected readonly solTxUrl = solTxUrl;
  /** Hashes render truncated; the copy button carries the full value. */
  protected readonly short = short;
  /** Absolute timestamps render in the viewer's local zone, no suffix. */
  protected readonly localTime = localTime;
  /** Wire durations render humanized; a missing duration renders a dash. */
  protected readonly humanDuration = humanDuration;

  /** The chart series is sliced to the selected range so the range buttons
   *  actually change what is plotted. In mock mode the fixture series stands
   *  in for real usage (the offline demo); in live mode it is bucketed from
   *  the store's real rows, so the chart can never show spending the request
   *  log beside it does not carry. `environment.useMock` is read at
   *  computation time; it is only mutated by specs in `beforeAll`, before
   *  the component is created. */
  protected readonly daily = computed(() =>
    environment.useMock
      ? DAILY_SPEND.slice(-this.store.usageDays())
      : bucketDaily(
          this.store.usage(),
          this.store.usageDays(),
          Date.now(),
          (r) => r.credits,
        ),
  );
  protected readonly requestsSlice = computed(() =>
    environment.useMock
      ? DAILY_REQUESTS.slice(-this.store.usageDays())
      : bucketDaily(
          this.store.usage(),
          this.store.usageDays(),
          Date.now(),
          () => 1,
        ),
  );
  /** The epoch millis of the first bar in the current slice. */
  protected readonly dayZero = computed(() =>
    environment.useMock
      ? DAY_ZERO + (DAILY_SPEND.length - this.daily().length) * DAY_MS
      : dayZeroFor(this.store.usageDays(), Date.now()),
  );
  /** Real mode with zero rows: the empty condition that makes the chart
   *  render the same "No usage yet" state as the request log. True when
   *  the chart should be replaced by that block. */
  protected readonly hasUsage = computed(
    () => !environment.useMock && this.store.usage().length === 0,
  );
  /** Spend stats over the current slice; null when the range is empty. */
  protected readonly stats = computed(() => periodStats(this.daily()));
  /** Request stats over the current slice; null when the range is empty. */
  protected readonly requestStats = computed(() =>
    periodStats(this.requestsSlice()),
  );
  /** Mean credits per request across the slice. */
  protected readonly creditsPerRequest = computed(() => {
    const s = this.stats();
    const r = this.requestStats();
    if (s === null || r === null || r.total === 0) return null;
    return s.total / r.total;
  });
  /** The calendar date of the peak day, e.g. "Mon 17 Aug". */
  protected readonly peakDateText = computed<string | null>(() => {
    const s = this.stats();
    if (s === null) return null;
    const t = new Date(this.dayZero() + s.peakIndex * DAY_MS);
    return new Intl.DateTimeFormat('en-US', {
      weekday: 'short',
      day: 'numeric',
      month: 'short',
    }).format(t);
  });

  /** The three status chips, in display order. */
  protected readonly filters: { key: UsageStatusFilter; label: string }[] = [
    { key: 'all', label: 'All' },
    { key: 'metered', label: 'Metered' },
    { key: 'error', label: 'Error' },
  ];

  protected readonly ranges = ['7 d', '30 d', '90 d'];
  protected readonly currentRange = computed(
    () => `${this.store.usageDays()} d`,
  );

  // ---- structured log filters ------------------------------------------
  /** The key dropdown's options: an All entry plus the wire's distinct
   *  prefixes. The facets are normalized at the api boundary (no '' or
   *  duplicate entries), so exactly one '' - one All option - reaches the
   *  dropdown, and the filter never offers a value the account has no rows
   *  under. */
  protected readonly keyOptions = computed<string[]>(() => [
    '',
    ...this.store.usageFacets().prefixes,
  ]);
  /** The model dropdown's options: an All entry plus the wire's distinct
   *  model ids, same single-All guarantee as the key options. */
  protected readonly modelOptions = computed<string[]>(() => [
    '',
    ...this.store.usageFacets().models,
  ]);
  /** The labels the shared dropdown renders for its '' placeholder entry. */
  protected readonly keyPlaceholder = 'All keys';
  protected readonly modelPlaceholder = 'All models';
  /** The from/to date inputs read as local calendar dates, '' when unset. */
  protected readonly fromValue = computed(() =>
    secToDateInput(this.store.usageFrom()),
  );
  protected readonly toValue = computed(() =>
    secToDateInput(this.store.usageTo()),
  );
  /** Whether any structured filter or the status chip is narrowing the log,
   *  so the empty state can tell "no match" from "no usage at all" apart and
   *  offer a one-action clear. */
  protected readonly filtersActive = computed(
    () =>
      this.store.usageStatusFilter() !== 'all' ||
      this.store.usageKey() !== '' ||
      this.store.usageModel() !== '' ||
      this.store.usageFrom() !== 0 ||
      this.store.usageTo() !== 0,
  );
  /** The active filters named in the empty state, joined with commas. */
  protected readonly activeFilterText = computed(() => {
    const parts: string[] = [];
    if (this.store.usageKey() !== '') parts.push(`key ${this.store.usageKey()}`);
    if (this.store.usageModel() !== '') parts.push(`model ${this.store.usageModel()}`);
    if (this.store.usageFrom() !== 0 || this.store.usageTo() !== 0) {
      const f = secToDateInput(this.store.usageFrom());
      const t = secToDateInput(this.store.usageTo());
      parts.push(`from ${f || 'start'} to ${t || 'now'}`);
    }
    const chip = this.store.usageStatusFilter();
    if (chip !== 'all') parts.push(chip === 'metered' ? 'metered' : 'errors');
    return parts.join(', ');
  });

  // ---- pagination ------------------------------------------------------
  protected readonly currentPage = computed(() => this.store.usagePage());
  protected readonly totalPages = computed(() => this.store.usageTotalPages());
  protected readonly canPrev = computed(() => this.currentPage() > 1);
  protected readonly canNext = computed(
    () => this.currentPage() < this.totalPages(),
  );

  protected onRange(r: string): void {
    const days = RANGE_DAYS[r];
    if (days !== undefined) this.store.usageDays.set(days);
  }

  /** Switch the status chip; Metered narrows client-side, All/Error re-fetch. */
  protected pickFilter(key: UsageStatusFilter): void {
    this.store.setUsageFilter(key);
  }

  /** True when this chip is the active one. */
  protected isActive(key: UsageStatusFilter): boolean {
    return this.store.usageStatusFilter() === key;
  }

  protected onKeySelected(value: string): void {
    this.store.setUsageKey(value);
  }

  protected onModelSelected(value: string): void {
    this.store.setUsageModel(value);
  }

  protected onFrom(event: Event): void {
    this.store.setUsageFrom(fromStartSec((event.target as HTMLInputElement).value));
  }

  protected onTo(event: Event): void {
    this.store.setUsageTo(toEndSec((event.target as HTMLInputElement).value));
  }

  /** One action clears every structured filter and the status chip. */
  protected clearFilters(): void {
    this.store.clearUsageFilters();
  }

  protected prevPage(): void {
    this.store.gotoPage(this.currentPage() - 1);
  }

  protected nextPage(): void {
    this.store.gotoPage(this.currentPage() + 1);
  }

  // ---- row settlement detail -------------------------------------------
  /** The one expanded request log row; null when none is open. */
  protected readonly expandedRow = signal<string | null>(null);

  /** Open the row's settlement detail, or close it when it is already open.
   *  Metered rows only: error rows are never charged and offer no detail. */
  protected toggleRow(r: UsageRow): void {
    this.expandedRow.set(this.expandedRow() === r.id ? null : r.id);
  }

  /** The row's inclusion-proof check, if a verify has run on it. */
  protected proofCheck(r: UsageRow): ProofCheck | undefined {
    return this.store.proofChecks()[r.id];
  }
}
