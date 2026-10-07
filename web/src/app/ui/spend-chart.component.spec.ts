import { TestBed } from '@angular/core/testing';
import { SpendChartComponent } from './spend-chart.component';

/** Thirty values: the demo's daily-spend series shape, literals so the spec
 *  has no mock-data dependency. */
const VALUES = Array.from({ length: 30 }, (_, i) => 8_000 + ((i * 3_797) % 24_000));

describe('SpendChartComponent container sizing', () => {
  function setup() {
    TestBed.configureTestingModule({ imports: [SpendChartComponent] });
    const fixture = TestBed.createComponent(SpendChartComponent);
    fixture.componentRef.setInput('values', VALUES);
    fixture.componentRef.setInput('dayZero', Date.UTC(2026, 6, 24));
    const component = fixture.componentInstance as unknown as {
      containerWidth: { set: (w: number) => void };
      scale: () => number;
      axisFont: () => number;
      axisStep: () => number;
      axisLabels: () => unknown[];
    };
    return { fixture, component };
  }

  it('pins the desktop baseline: full scale, default font, every-6th label', () => {
    const { component } = setup();
    component.containerWidth.set(900);
    expect(component.scale()).toBeCloseTo(1, 5);
    expect(component.axisFont()).toBe(11.5);
    expect(component.axisStep()).toBe(6);
    // i = 0, 6, 12, 18, 24 plus the last bar (i = 29).
    expect(component.axisLabels().length).toBe(6);
  });

  it('counter-scales the axis font and keeps the label gap at 343px', () => {
    const { component } = setup();
    component.containerWidth.set(343);
    // 343 / 900 = 0.3811...
    expect(component.scale()).toBeCloseTo(0.381, 2);
    // 11.5 / 0.3811 rounded to a tenth of a viewBox unit.
    expect(component.axisFont()).toBe(30.2);
    // The per-bar gap at this width still clears 56px with step 6.
    expect(component.axisStep()).toBe(6);
    expect(component.axisLabels().length).toBe(6);
  });

  it('thins out labels as the container shrinks below 343px', () => {
    const { component } = setup();
    component.containerWidth.set(343);
    const at343 = component.axisLabels().length;
    component.containerWidth.set(200);
    expect(component.axisStep()).toBe(9);
    expect(component.axisLabels().length).toBeLessThan(at343);
  });

  it('publishes the counter-scaled font on the chart root and renders one x-label per entry', () => {
    const { fixture, component } = setup();
    component.containerWidth.set(343);
    fixture.detectChanges();
    const chart = (fixture.nativeElement as unknown as HTMLElement).querySelector('.chart')!;
    expect(chart.getAttribute('style')).toContain('--chart-axis-font');
    // Only the x-axis labels are text-anchor="middle"; the grid and mean
    // labels anchor "end", so this counts exactly the thinned series.
    const middle = chart.querySelectorAll('text.axis[text-anchor="middle"]');
    expect(middle.length).toBe(component.axisLabels().length);
  });
});
