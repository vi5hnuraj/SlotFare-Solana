import { TestBed } from '@angular/core/testing';
import { NETWORK } from '../core/runtime-config';
import { InferenceNoticeComponent } from './inference-notice.component';

describe('InferenceNoticeComponent', () => {
  it('renders the NVIDIA free API disclaimer on testnet (the compiled default)', () => {
    TestBed.configureTestingModule({ imports: [InferenceNoticeComponent] });
    const fixture = TestBed.createComponent(InferenceNoticeComponent);
    fixture.detectChanges();

    const notice = fixture.nativeElement.querySelector('.notice');
    expect(notice).not.toBeNull();
    expect(notice.textContent).toContain("NVIDIA's free API");
    expect(notice.textContent).toContain('demonstration only');
  });

  it('renders nothing on mainnet', () => {
    TestBed.configureTestingModule({
      imports: [InferenceNoticeComponent],
      providers: [{ provide: NETWORK, useValue: 'mainnet' }],
    });
    const fixture = TestBed.createComponent(InferenceNoticeComponent);
    fixture.detectChanges();

    expect(fixture.nativeElement.querySelector('.notice')).toBeNull();
  });
});
