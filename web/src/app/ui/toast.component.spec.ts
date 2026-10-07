import { TestBed } from '@angular/core/testing';
import { ToastService } from '../core/toast';
import { ToastComponent } from './toast.component';

describe('ToastComponent', () => {
  it('is a status region that lights up on ToastService.say', () => {
    TestBed.configureTestingModule({
      imports: [ToastComponent],
      providers: [ToastService],
    });
    const fixture = TestBed.createComponent(ToastComponent);
    fixture.detectChanges();

    const toast = fixture.nativeElement.querySelector('.toast') as HTMLElement;
    expect(toast).toBeTruthy();
    expect(toast.getAttribute('role')).toBe('status');
    expect(toast.classList).not.toContain('on');

    TestBed.inject(ToastService).say('Copied to clipboard');
    fixture.detectChanges();
    expect(toast.classList).toContain('on');
    expect(toast.textContent).toContain('Copied to clipboard');
  });
});
