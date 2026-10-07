import { Component, inject } from '@angular/core';
import { IconComponent } from './icon.component';
import { ToastService } from '../core/toast';

@Component({
  selector: 'slf-toast',
  imports: [IconComponent],
  template: `
    <div class="toast" [class.on]="toast.message() !== null" role="status"
         aria-live="polite">
      <slf-icon name="check" [width]="2.5" />
      <span>{{ toast.message() }}</span>
    </div>
  `,
})
export class ToastComponent {
  protected readonly toast = inject(ToastService);
}
