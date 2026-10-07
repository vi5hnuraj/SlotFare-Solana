import { Component, inject, input } from '@angular/core';
import { IconComponent } from './icon.component';
import { ToastService } from '../core/toast';

/** Copies a literal value. The value is passed in rather than scraped out
 *  of the DOM, so what lands on the clipboard is what the caller meant. */
@Component({
  selector: 'slf-copy-button',
  imports: [IconComponent],
  template: `
    @if (label()) {
      <button class="btn btn-secondary btn-sm" type="button"
              (click)="copy()" [attr.aria-label]="aria()">
        <slf-icon name="copy" />
        {{ label() }}
      </button>
    } @else {
      <button class="icon-btn" type="button" (click)="copy()"
              [attr.aria-label]="aria()">
        <slf-icon name="copy" />
      </button>
    }
  `,
})
export class CopyButtonComponent {
  private readonly toast = inject(ToastService);
  readonly value = input.required<string>();
  readonly label = input('');
  readonly aria = input('Copy');

  protected copy(): void {
    void this.toast.copy(this.value());
  }
}
