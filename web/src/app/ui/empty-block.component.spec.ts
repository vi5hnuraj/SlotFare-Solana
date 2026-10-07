import { TestBed } from '@angular/core/testing';
import { EmptyBlockComponent } from './empty-block.component';

describe('EmptyBlockComponent', () => {
  it('renders the title and detail', () => {
    TestBed.configureTestingModule({ imports: [EmptyBlockComponent] });
    const fixture = TestBed.createComponent(EmptyBlockComponent);
    fixture.componentRef.setInput('title', 'No requests yet');
    fixture.componentRef.setInput('detail', 'Requests appear here soon.');
    fixture.detectChanges();

    const block = fixture.nativeElement.querySelector('.state-block');
    expect(block.querySelector('h4').textContent).toBe('No requests yet');
    expect(block.querySelector('p').textContent).toBe('Requests appear here soon.');
  });

  it('omits the detail line when none is given', () => {
    TestBed.configureTestingModule({ imports: [EmptyBlockComponent] });
    const fixture = TestBed.createComponent(EmptyBlockComponent);
    fixture.componentRef.setInput('title', 'Nothing here');
    fixture.detectChanges();

    const block = fixture.nativeElement.querySelector('.state-block');
    expect(block.querySelector('h4').textContent).toBe('Nothing here');
    expect(block.querySelector('p')).toBeNull();
  });

  it('shows the .mark only when an icon is passed', () => {
    TestBed.configureTestingModule({ imports: [EmptyBlockComponent] });
    const fixture = TestBed.createComponent(EmptyBlockComponent);
    fixture.componentRef.setInput('title', 'No active keys');
    fixture.detectChanges();
    expect(
      fixture.nativeElement.querySelector('.state-block .mark'),
    ).toBeNull();

    fixture.componentRef.setInput('icon', 'key');
    fixture.detectChanges();
    expect(
      fixture.nativeElement.querySelector('.state-block .mark svg'),
    ).not.toBeNull();
  });
});
