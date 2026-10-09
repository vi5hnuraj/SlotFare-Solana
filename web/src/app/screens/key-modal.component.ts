import { Component, inject, signal } from '@angular/core';
import { ConsoleStore } from '../core/console-store';
import { IconComponent } from '../ui/icon.component';
import { CopyButtonComponent } from '../ui/copy-button.component';

/** Two-step create dialog. The plaintext is minted server-side, held in a
 *  signal, shown once, and discarded the moment the dialog closes. */
@Component({
  selector: 'slf-key-modal',
  imports: [IconComponent, CopyButtonComponent],
  templateUrl: './key-modal.component.html',
})
export class KeyModalComponent {
  private readonly store = inject(ConsoleStore);

  protected readonly open = signal(false);
  protected readonly revealed = signal(false);
  protected readonly label = signal('');
  protected readonly plaintext = signal('');
  protected readonly stored = signal(false);
  protected readonly creating = signal(false);
  protected readonly createError = signal<string | null>(null);

  protected readonly prefix = () => this.plaintext().slice(0, 13);

  show(): void {
    this.label.set('');
    this.plaintext.set('');
    this.revealed.set(false);
    this.stored.set(false);
    this.createError.set(null);
    this.open.set(true);
  }

  protected close(): void {
    // The single place the plaintext is discarded.
    this.plaintext.set('');
    this.revealed.set(false);
    this.stored.set(false);
    this.open.set(false);
  }

  protected onLabel(event: Event): void {
    this.label.set((event.target as HTMLInputElement).value);
  }

  protected onStored(event: Event): void {
    this.stored.set((event.target as HTMLInputElement).checked);
  }

  protected async create(): Promise<void> {
    const label = this.label().trim() || 'untitled-key';
    this.label.set(label);
    this.creating.set(true);
    this.createError.set(null);
    try {
      const created = await this.store.createKey(label);
      this.plaintext.set(created.plaintext);
      this.revealed.set(true);
    } catch (e) {
      this.createError.set(e instanceof Error ? e.message : 'Could not create key');
    } finally {
      this.creating.set(false);
    }
  }

  protected done(): void {
    this.close();
  }

  protected onScrimClick(event: MouseEvent): void {
    if (event.target === event.currentTarget) this.close();
  }

  protected onKeydown(event: KeyboardEvent): void {
    if (event.key === 'Escape') this.close();
  }
}
