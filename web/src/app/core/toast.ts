import { Service, signal } from '@angular/core';

@Service()
export class ToastService {
  readonly message = signal<string | null>(null);
  private timer: ReturnType<typeof setTimeout> | undefined;

  say(message: string): void {
    this.message.set(message);
    clearTimeout(this.timer);
    this.timer = setTimeout(() => this.message.set(null), 2200);
  }

  /** Copy text and confirm it, falling back quietly where the API is absent. */
  async copy(text: string): Promise<void> {
    try {
      await navigator.clipboard?.writeText(text);
    } catch {
      /* clipboard blocked: still tell the user, the value is on screen */
    }
    this.say('Copied to clipboard');
  }
}
