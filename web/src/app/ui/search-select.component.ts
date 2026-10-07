import {
  Component,
  DestroyRef,
  ElementRef,
  computed,
  inject,
  input,
  output,
  signal,
} from '@angular/core';
import { IconComponent } from './icon.component';

/** Per-instance id prefix so several open dropdowns never share option ids. */
let uid = 0;

/**
 * The shared searchable dropdown: a button trigger that opens a listbox of
 * options, type-to-filter on the trigger itself (no separate text box), full
 * keyboard operation. The '' placeholder value renders as the supplied
 * placeholder label; consumers own that entry in their options list.
 */
@Component({
  selector: 'slf-search-select',
  imports: [IconComponent],
  template: `
    <div class="sd" [class.open]="open()">
      <button
        type="button"
        class="sd-trigger"
        role="combobox"
        aria-haspopup="listbox"
        [attr.aria-expanded]="open()"
        [attr.aria-controls]="listId()"
        [attr.aria-labelledby]="labelledby()"
        (click)="toggle()"
        (keydown)="onKeydown($event)"
      >
        <span class="sd-value">{{ displayLabel() }}</span>
        <slf-icon class="sd-chevron" name="chevron" />
      </button>
      @if (open()) {
        <div class="sd-list" role="listbox" [id]="listId()"
             [attr.aria-activedescendant]="activeId()">
          @if (filtered().length === 0) {
            <div class="sd-no-match">No matches</div>
          } @else {
            @for (o of filtered(); track o; let i = $index) {
              <div class="sd-opt" role="option" [id]="optId(i)"
                   [class.is-active]="i === active()"
                   [attr.aria-selected]="o === value()"
                   (click)="pick(o)">
                <span class="opt-label">{{ labelFor(o) }}</span>
                @if (o === value()) {
                  <slf-icon class="opt-check" name="check" />
                }
              </div>
            }
          }
        </div>
      }
    </div>
  `,
})
export class SearchSelectComponent {
  /** Option values in display order; includes the '' placeholder entry. */
  readonly options = input.required<string[]>();
  /** The currently selected value ('' for the placeholder). */
  readonly value = input.required<string>();
  /** The label rendered for the '' placeholder value. */
  readonly placeholder = input.required<string>();
  /** Id of the visible label element, wired as aria-labelledby. */
  readonly labelledby = input<string>('');
  /** Emits the picked value when an option is selected. */
  readonly change = output<string>();

  protected readonly open = signal(false);
  /** The type-to-filter query, lowercased; empty shows every option. */
  private readonly query = signal('');
  /** Highlighted index within the filtered options. */
  protected readonly active = signal(0);

  private readonly host = inject(ElementRef);
  private readonly uid = ++uid;
  private docListener: ((e: MouseEvent) => void) | null = null;

  constructor() {
    // A click outside the control closes the list without changing the value.
    inject(DestroyRef).onDestroy(() => this.removeDocListener());
  }

  protected readonly listId = computed(() => `sd-list-${this.uid}`);
  /** Case-insensitive substring match over the rendered labels; the
   *  placeholder label counts like any other option's. */
  protected readonly filtered = computed(() => {
    const q = this.query();
    if (q === '') return this.options();
    return this.options().filter((o) => this.labelFor(o).toLowerCase().includes(q));
  });
  /** What the trigger shows while closed and open: the placeholder label
   *  for '', the value text otherwise. */
  protected readonly displayLabel = computed(() =>
    this.value() === '' ? this.placeholder() : this.value(),
  );
  protected optId(i: number): string {
    return `sd-opt-${this.uid}-${i}`;
  }
  protected activeId(): string | null {
    return this.filtered().length === 0 ? null : this.optId(this.active());
  }
  protected labelFor(o: string): string {
    return o === '' ? this.placeholder() : o;
  }

  protected toggle(): void {
    if (this.open()) this.close();
    else this.openList();
  }

  private openList(): void {
    // Keyboard focus lands on the selected option when there is one, else
    // the first option.
    const idx = this.options().indexOf(this.value());
    this.active.set(idx >= 0 ? idx : 0);
    this.open.set(true);
    if (this.docListener === null) {
      this.docListener = (e: MouseEvent) => {
        // Clicks inside the control (trigger or option rows) are handled by
        // their own listeners; only outside clicks close the list.
        if (!this.host.nativeElement.contains(e.target as Node)) this.close();
      };
      document.addEventListener('click', this.docListener);
    }
  }

  private close(): void {
    this.open.set(false);
    this.query.set('');
    this.active.set(0);
    this.removeDocListener();
  }

  private removeDocListener(): void {
    if (this.docListener !== null) {
      document.removeEventListener('click', this.docListener);
      this.docListener = null;
    }
  }

  protected onKeydown(e: KeyboardEvent): void {
    if (this.open()) {
      switch (e.key) {
        case 'ArrowDown':
          e.preventDefault();
          this.moveActive(1);
          break;
        case 'ArrowUp':
          e.preventDefault();
          this.moveActive(-1);
          break;
        case 'Home':
          e.preventDefault();
          this.active.set(0);
          break;
        case 'End':
          e.preventDefault();
          this.active.set(Math.max(0, this.filtered().length - 1));
          break;
        case 'Enter':
        case ' ':
          e.preventDefault();
          this.pickHighlighted();
          break;
        case 'Escape':
          e.preventDefault();
          this.close();
          break;
        case 'Backspace':
          e.preventDefault();
          this.query.update((q) => q.slice(0, -1));
          break;
        default:
          // Printable characters append to the filter; each keystroke resets
          // the highlight to the first match.
          if (e.key.length === 1 && !e.ctrlKey && !e.metaKey && !e.altKey) {
            e.preventDefault();
            this.query.update((q) => q + e.key.toLowerCase());
            this.active.set(0);
          }
      }
      return;
    }
    // Closed: Enter/Space/arrow keys open the list at the current selection.
    switch (e.key) {
      case 'Enter':
      case ' ':
      case 'ArrowDown':
      case 'ArrowUp':
        e.preventDefault();
        this.openList();
        break;
    }
  }

  private moveActive(delta: number): void {
    const n = this.filtered().length;
    if (n === 0) return;
    this.active.set(((this.active() + delta) % n + n) % n);
  }

  private pickHighlighted(): void {
    const o = this.filtered()[this.active()];
    if (o !== undefined) this.pick(o);
  }

  /** Pick an option: emit it and close, keeping focus on the trigger. */
  protected pick(o: string): void {
    this.change.emit(o);
    this.close();
  }
}
