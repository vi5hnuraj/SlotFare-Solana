import { TestBed } from '@angular/core/testing';
import { ErrorBlockComponent } from './error-block.component';

describe('ErrorBlockComponent', () => {
  it('renders the message inside the state block', () => {
    TestBed.configureTestingModule({ imports: [ErrorBlockComponent] });
    const fixture = TestBed.createComponent(ErrorBlockComponent);
    fixture.componentRef.setInput('message', 'The balance could not be refreshed.');
    fixture.detectChanges();

    const block = fixture.nativeElement.querySelector('.state-block');
    expect(block).not.toBeNull();
    expect(block.textContent).toContain('The balance could not be refreshed.');
    expect(block.textContent).toContain('Something went wrong');
  });

  it('clicking Retry emits retry once per click', () => {
    TestBed.configureTestingModule({ imports: [ErrorBlockComponent] });
    const fixture = TestBed.createComponent(ErrorBlockComponent);
    fixture.componentRef.setInput('message', 'Nope.');
    let clicks = 0;
    fixture.componentRef.instance.retry.subscribe(() => clicks += 1);
    fixture.detectChanges();

    const buttons = Array.from(
      fixture.nativeElement.querySelectorAll('.state-block button'),
    ) as HTMLButtonElement[];
    const retry = buttons.find((b) => b.textContent?.trim() === 'Retry');
    expect(retry).toBeDefined();
    retry!.click();
    retry!.click();
    expect(clicks).toBe(2);
  });

  it('clicking the X emits dismiss, not retry', () => {
    TestBed.configureTestingModule({ imports: [ErrorBlockComponent] });
    const fixture = TestBed.createComponent(ErrorBlockComponent);
    fixture.componentRef.setInput('message', 'Nope.');
    let dismissed = 0;
    let retried = 0;
    fixture.componentRef.instance.dismiss.subscribe(() => dismissed += 1);
    fixture.componentRef.instance.retry.subscribe(() => retried += 1);
    fixture.detectChanges();

    const close = fixture.nativeElement.querySelector(
      '.state-close',
    ) as HTMLButtonElement;
    expect(close.getAttribute('aria-label')).toBe('Dismiss');
    close.click();
    expect(dismissed).toBe(1);
    expect(retried).toBe(0);
  });

  it('hides the retry button when showRetry is false but keeps the X', () => {
    TestBed.configureTestingModule({ imports: [ErrorBlockComponent] });
    const fixture = TestBed.createComponent(ErrorBlockComponent);
    fixture.componentRef.setInput('message', 'Nope.');
    fixture.componentRef.setInput('showRetry', false);
    fixture.detectChanges();

    const buttons = Array.from(
      fixture.nativeElement.querySelectorAll('.state-block button'),
    ) as HTMLButtonElement[];
    expect(buttons.length).toBe(1);
    expect(buttons[0].classList.contains('state-close')).toBe(true);
  });
});
