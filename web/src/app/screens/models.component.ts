import { Component, computed, inject, signal } from '@angular/core';
import { RouterLink } from '@angular/router';
import { BreakpointService } from '../core/breakpoint';
import {
  activeFilterCount,
  applyFilters,
  CAPABILITY_LABELS,
  clearFilters,
  deriveVendors,
  describeActiveFilters,
  ModelFilterState,
  VendorChip,
} from '../core/model-filters';
import { ModelCapability } from '../core/models';
import { ConsoleStore } from '../core/console-store';
import { contextShort, pricingLines, vendorLogo, vendorName } from '../core/model-card';
import { SkeletonComponent } from '../ui/skeleton.component';
import { ErrorBlockComponent } from '../ui/error-block.component';
import { InferenceNoticeComponent } from '../ui/inference-notice.component';

/**
 * The dedicated Models page: every model the wire serves as a linkable row,
 * with two filter axes only: vendor chips (derived, with counts) and the
 * search box. No context buckets and no other facets: this is a browse page,
 * not the endpoint screen's picker.
 */
@Component({
  selector: 'slf-models',
  imports: [RouterLink, SkeletonComponent, ErrorBlockComponent, InferenceNoticeComponent],
  templateUrl: './models.component.html',
})
export class ModelsComponent {
  protected readonly store = inject(ConsoleStore);
  private readonly breakpoints = inject(BreakpointService);
  protected readonly bp = this.breakpoints.bp;

  protected readonly capLabel = (c: ModelCapability): string => CAPABILITY_LABELS[c];
  protected readonly contextShort = contextShort;
  protected readonly pricingLines = pricingLines;
  protected readonly vendorLogo = vendorLogo;
  protected readonly vendorName = vendorName;

  /** The catalog straight off the wire; empty until it loads or on failure. */
  protected readonly models = computed(() => this.store.models());

  /** Only the two axes this page exposes: search and vendor chips. */
  protected readonly state = signal<ModelFilterState>(clearFilters());
  /** Mobile only: the vendor row collapses behind this toggle. */
  protected readonly sheetOpen = signal(false);

  protected readonly vendors = computed((): VendorChip[] =>
    deriveVendors(this.models()),
  );
  protected readonly activeCount = computed(() => activeFilterCount(this.state()));
  protected readonly filtered = computed(() => applyFilters(this.models(), this.state()));
  protected readonly emptyReason = computed(() => describeActiveFilters(this.state()));

  protected onSearch(value: string): void {
    this.state.update((s) => ({ ...s, search: value }));
  }

  protected toggleVendor(label: string): void {
    this.state.update((s) => {
      const next = s.vendors.includes(label)
        ? s.vendors.filter((v) => v !== label)
        : [...s.vendors, label];
      return { ...s, vendors: next };
    });
  }

  /** One action: resets the search and every chip. */
  protected clearAll(): void {
    this.state.set(clearFilters());
    this.sheetOpen.set(false);
  }
}
