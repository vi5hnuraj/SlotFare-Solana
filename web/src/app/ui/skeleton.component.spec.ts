import { ComponentFixture, TestBed } from '@angular/core/testing';
import { SkeletonComponent } from './skeleton.component';

describe('SkeletonComponent', () => {
  function setup(rows?: number): ComponentFixture<SkeletonComponent> {
    TestBed.configureTestingModule({ imports: [SkeletonComponent] });
    const fixture = TestBed.createComponent(SkeletonComponent);
    if (rows !== undefined) fixture.componentRef.setInput('rows', rows);
    fixture.detectChanges();
    return fixture;
  }

  it('renders one .skeleton bar per row', () => {
    const fixture = setup(5);
    expect(fixture.nativeElement.querySelectorAll('.skeleton')).toHaveLength(5);
  });

  it('defaults to three rows', () => {
    const fixture = setup();
    expect(fixture.nativeElement.querySelectorAll('.skeleton')).toHaveLength(3);
  });

  it('announces itself as loading status', () => {
    const fixture = setup();
    const host = fixture.nativeElement.querySelector('[role="status"]');
    expect(host).not.toBeNull();
    expect(host.getAttribute('aria-label')).toBe('Loading');
  });
});
