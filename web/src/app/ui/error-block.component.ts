import { Component, input, output } from '@angular/core';
import { IconComponent } from './icon.component';

/**
 * Says what failed and offers a retry. The retry click only emits; which
 * loader to re-run is up to the parent. The X only emits too; which signal
 * to clear is up to the parent, so no card can outlive its dismiss.
 */
@Component({
  selector: 'slf-error-block',
  imports: [IconComponent],
  template: `
    <div class="state-block">
      <button class="icon-btn state-close" type="button" aria-label="Dismiss"
              (click)="dismiss.emit()">
        <slf-icon name="close" />
      </button>
      <div class="mark"><slf-icon name="close" /></div>
      <h4>{{ title() }}</h4>
      <p>{{ message() }}</p>
      @if (showRetry()) {
        <div class="actions">
          <button class="btn btn-secondary btn-sm" type="button"
                  (click)="retry.emit()">
            Retry
          </button>
        </div>
      }
    </div>
  `,
})
export class ErrorBlockComponent {
  readonly title = input('Something went wrong');
  readonly message = input.required<string>();
  /** Some failures have no in-page remedy (e.g. no wallet installed); hide
   *  the retry button for those. */
  readonly showRetry = input(true);
  readonly retry = output<void>();
  /** The X control: clears whatever card this block is showing. */
  readonly dismiss = output<void>();
}
