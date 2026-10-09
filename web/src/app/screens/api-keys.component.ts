import { Component, computed, inject, viewChild } from '@angular/core';
import { ConsoleStore } from '../core/console-store';
import { ToastService } from '../core/toast';
import { num } from '../core/pricing';
import { buildCurl, buildRpcCurl } from '../core/model-filters';
import { localTime, relativeTime } from '../core/relative-time';
import { IconComponent } from '../ui/icon.component';
import { CopyButtonComponent } from '../ui/copy-button.component';
import { SkeletonComponent } from '../ui/skeleton.component';
import { ErrorBlockComponent } from '../ui/error-block.component';
import { EmptyBlockComponent } from '../ui/empty-block.component';
import { KeyModalComponent } from './key-modal.component';

/** The model id the curl example carries when no catalog has loaded: an
 *  explicit placeholder, not a fabricated model id. The user replaces it
 *  with the model their key is routed to; the "catalog unavailable" hint
 *  says so. */
const MODEL_PLACEHOLDER = '<model-id>';

@Component({
  selector: 'slf-api-keys',
  imports: [IconComponent, CopyButtonComponent, SkeletonComponent, ErrorBlockComponent, EmptyBlockComponent, KeyModalComponent],
  templateUrl: './api-keys.component.html',
})
export class ApiKeysComponent {
  private readonly toast = inject(ToastService);
  protected readonly store = inject(ConsoleStore);
  protected readonly num = num;
  /** The inference base URL: the wire-advertised value, or the page origin
   *  with the documented /v1 path when the wire carries none. */
  protected readonly baseUrl = computed(() => this.store.endpointBaseUrl());
  /** The selected model; null when no catalog has loaded. */
  protected readonly selected = computed(() => this.store.selectedModel());
  /** The model id the curl example carries: the selection, else the placeholder. */
  protected readonly curlModelId = computed(
    () => this.selected()?.id ?? MODEL_PLACEHOLDER,
  );
  /** True when the wire advertises no inference base URL: the deployment
   *  runs without an inference upstream, so only the /rpc proxy is live. */
  protected readonly rpcExample = computed(
    () => this.store.advertisedBaseUrl() === '',
  );
  /** The example's base URL: the /rpc proxy when inference is off, the
   *  advertised inference base otherwise (page origin with /v1 as fallback). */
  protected readonly exampleBase = computed(() =>
    this.rpcExample() ? `${window.location.origin}/rpc` : this.baseUrl(),
  );
  /** The curl example: a metered Devnet RPC call when inference is off, the
   *  wire-advertised base URL and selected model's real id otherwise.
   *  The header references `$SLOTFARE_API_KEY`, exported once from a key.
   *  Until a catalog has loaded the placeholder model id stands in with a
   *  chat-shaped body. */
  protected readonly curl = computed(() => {
    if (this.rpcExample()) return buildRpcCurl(this.exampleBase());
    const m = this.selected();
    return buildCurl(
      this.baseUrl(),
      m ?? {
        id: MODEL_PLACEHOLDER,
        name: '',
        pricing: { uncachedInput: 0, cachedInput: 0, output: 0 },
      },
    );
  });
  /** Relative time for a key's last use; the only place it is computed. */
  protected readonly relTime = (ts: string | null): string => relativeTime(ts);
  /** Absolute timestamps render in the viewer's local zone, no suffix. */
  protected readonly localTime = localTime;
  private readonly modal = viewChild.required(KeyModalComponent);

  protected create(): void {
    this.modal().show();
  }

  protected async revoke(id: string): Promise<void> {
    try {
      await this.store.revokeKey(id);
      this.toast.say('Key revoked · requests with it now return 401');
    } catch (e) {
      this.toast.say(e instanceof Error ? e.message : 'Revoke failed');
    }
  }
}
