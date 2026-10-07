import { ComponentFixture, TestBed } from '@angular/core/testing';
import { SearchSelectComponent } from './search-select.component';

describe('SearchSelectComponent', () => {
  const OPTIONS = ['', 'llama-3.2-3b', 'qwen3-8-27b'];

  function setup(
    options: string[] = OPTIONS,
    value = '',
    placeholder = 'All models',
  ): ComponentFixture<SearchSelectComponent> {
    TestBed.configureTestingModule({ imports: [SearchSelectComponent] });
    const fixture = TestBed.createComponent(SearchSelectComponent);
    fixture.componentRef.setInput('options', options);
    fixture.componentRef.setInput('value', value);
    fixture.componentRef.setInput('placeholder', placeholder);
    fixture.detectChanges();
    return fixture;
  }

  function triggerOf(fixture: ComponentFixture<SearchSelectComponent>): HTMLElement {
    return fixture.nativeElement.querySelector('.sd-trigger') as HTMLElement;
  }

  function optionLabels(fixture: ComponentFixture<SearchSelectComponent>): string[] {
    const els = Array.from(
      fixture.nativeElement.querySelectorAll('.sd-opt .opt-label'),
    ) as Element[];
    return els.map((el) => (el.textContent ?? '').trim());
  }

  function key(el: HTMLElement, k: string): void {
    el.dispatchEvent(new KeyboardEvent('keydown', { key: k }));
  }

  it('is closed by default and shows the placeholder on the trigger', () => {
    const fixture = setup();
    expect(fixture.nativeElement.querySelector('.sd-list')).toBeNull();
    const trigger = triggerOf(fixture);
    expect(trigger.getAttribute('aria-expanded')).toBe('false');
    expect(trigger.textContent).toContain('All models');
  });

  it('opening renders one role=option per option, in order, with the selected marked', () => {
    const fixture = setup(OPTIONS, 'qwen3-8-27b');
    const trigger = triggerOf(fixture);
    trigger.click();
    fixture.detectChanges();

    expect(trigger.getAttribute('aria-expanded')).toBe('true');
    const opts = fixture.nativeElement.querySelectorAll('[role="option"]');
    expect(Array.from(opts).map((o) => (o as Element).textContent?.trim())).toEqual([
      'All models',
      'llama-3.2-3b',
      'qwen3-8-27b',
    ]);
    expect(opts[0].getAttribute('aria-selected')).toBe('false');
    expect(opts[1].getAttribute('aria-selected')).toBe('false');
    expect(opts[2].getAttribute('aria-selected')).toBe('true');
    // The selected option carries the check icon while the list is open.
    expect(opts[2].querySelector('.opt-check')).toBeTruthy();
    expect(opts[0].querySelector('.opt-check')).toBeNull();
  });

  it('type-to-filter narrows the list; a non-matching query shows No matches', () => {
    const fixture = setup(OPTIONS, '', 'All models');
    const trigger = triggerOf(fixture);
    trigger.click();
    fixture.detectChanges();

    // 'l' matches the placeholder label and llama; 'la' only llama, because
    // "all models" carries no 'la' substring; 'laz' matches nothing.
    key(trigger, 'l');
    fixture.detectChanges();
    expect(optionLabels(fixture)).toEqual(['All models', 'llama-3.2-3b']);
    key(trigger, 'a');
    fixture.detectChanges();
    expect(optionLabels(fixture)).toEqual(['llama-3.2-3b']);

    // Nothing matches: one muted row, no options.
    key(trigger, 'z');
    fixture.detectChanges();
    expect(fixture.nativeElement.querySelectorAll('[role="option"]')).toHaveLength(0);
    expect(fixture.nativeElement.querySelector('.sd-no-match')?.textContent).toContain('No matches');
  });

  it('ArrowDown then Enter picks the highlighted option, emits it and closes', () => {
    const fixture = setup(OPTIONS, '');
    const picked: string[] = [];
    fixture.componentInstance.change.subscribe((v) => picked.push(v));
    const trigger = triggerOf(fixture);

    key(trigger, 'Enter');
    fixture.detectChanges();
    expect(fixture.nativeElement.querySelector('.sd-list')).toBeTruthy();

    key(trigger, 'ArrowDown');
    fixture.detectChanges();
    key(trigger, 'ArrowDown');
    fixture.detectChanges();
    key(trigger, 'Enter');
    fixture.detectChanges();

    expect(picked).toEqual(['qwen3-8-27b']);
    expect(fixture.nativeElement.querySelector('.sd-list')).toBeNull();
  });

  it('Escape closes without emitting a change', () => {
    const fixture = setup(OPTIONS, '');
    const picked: string[] = [];
    fixture.componentInstance.change.subscribe((v) => picked.push(v));
    const trigger = triggerOf(fixture);

    trigger.click();
    fixture.detectChanges();
    key(trigger, 'Escape');
    fixture.detectChanges();

    expect(fixture.nativeElement.querySelector('.sd-list')).toBeNull();
    expect(picked).toEqual([]);
  });

  it('picking the placeholder option emits "" and restores the placeholder label', () => {
    const fixture = setup(OPTIONS, 'llama-3.2-3b');
    const picked: string[] = [];
    fixture.componentInstance.change.subscribe((v) => picked.push(v));
    const trigger = triggerOf(fixture);
    expect(trigger.textContent).toContain('llama-3.2-3b');

    trigger.click();
    fixture.detectChanges();
    // The highlight opens on the selected option; ArrowUp reaches ''.
    key(trigger, 'ArrowUp');
    fixture.detectChanges();
    key(trigger, 'Enter');
    fixture.detectChanges();

    expect(picked).toEqual(['']);
    fixture.componentRef.setInput('value', '');
    fixture.detectChanges();
    expect(trigger.textContent).toContain('All models');
  });
});
