import { TestBed } from '@angular/core/testing';
import { provideRouter } from '@angular/router';
import { PHASES, RoadmapComponent } from './roadmap.component';

describe('RoadmapComponent', () => {
  beforeEach(() => {
    TestBed.configureTestingModule({ imports: [RoadmapComponent], providers: [provideRouter([])] });
  });

  it('renders every phase in build order, with only the first marked Next', () => {
    const fixture = TestBed.createComponent(RoadmapComponent);
    fixture.detectChanges();
    const el: HTMLElement = fixture.nativeElement;
    const phases = Array.from(el.querySelectorAll('.rm-phase'));
    expect(phases.length).toBe(PHASES.length);
    phases.forEach((li, i) => {
      expect(li.querySelector('h3')?.textContent).toContain(PHASES[i].title);
      expect(li.querySelector('.pill')?.textContent?.trim()).toBe(i === 0 ? 'Next' : 'Planned');
    });
  });

  it('links only inside the page, never to an outside project', () => {
    const fixture = TestBed.createComponent(RoadmapComponent);
    fixture.detectChanges();
    const hrefs = Array.from(fixture.nativeElement.querySelectorAll('a[href]')).map((a) => (a as HTMLAnchorElement).getAttribute('href'));
    expect(hrefs.length).toBeGreaterThan(0);
    // In-page anchors and in-app routes only: no absolute URL can point off
    // to someone else's site.
    expect(hrefs.filter((h) => h !== null && !h.startsWith('#') && !h.startsWith('/'))).toEqual([]);
  });
});
