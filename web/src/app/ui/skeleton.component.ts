import { Component, computed, input, numberAttribute } from '@angular/core';

/**
 * A quiet placeholder for content that is still loading: one .skeleton bar
 * per row, occupying the same space as what it replaces. No spinner.
 */
@Component({
  selector: 'slf-skeleton',
  template: `
    <div role="status" aria-label="Loading" style="display:grid;gap:10px">
      @for (i of bars(); track i) {
        <div class="skeleton" style="height:12px"></div>
      }
    </div>
  `,
})
export class SkeletonComponent {
  readonly rows = input(3, { transform: numberAttribute });
  protected readonly bars = computed(() =>
    Array.from({ length: this.rows() }, (_, i) => i),
  );
}
