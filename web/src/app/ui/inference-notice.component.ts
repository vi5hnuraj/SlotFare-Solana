import { Component, inject } from '@angular/core';
import { NETWORK } from '../core/runtime-config';
import { IconComponent } from './icon.component';

/**
 * The testnet inference disclaimer. On testnet slotfared proxies model calls to
 * NVIDIA's free API (build.nvidia.com), a demo convenience with no service
 * level: calls can hang, time out or fail account-wide without notice. The
 * card says so where a user decides to call a model. It renders nothing off
 * testnet, so the same template ships to mainnet unchanged.
 */
@Component({
  selector: 'slf-inference-notice',
  imports: [IconComponent],
  template: `
    @if (network === 'testnet') {
      <div class="notice notice-alert inference-notice" role="note">
        <slf-icon name="info" [width]="1.8" />
        <span>
          <b>Testnet inference runs on NVIDIA's free API.</b> This deployment
          is for demonstration only: model calls are proxied to
          build.nvidia.com's free tier, so responses can be slow, time out or
          fail without notice. Metering and settlement are real; a request
          that fails upstream is not charged.
        </span>
      </div>
    }
  `,
})
export class InferenceNoticeComponent {
  protected readonly network = inject(NETWORK);
}
