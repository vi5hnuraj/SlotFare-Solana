import { CREDITS_PER_USD, RATE, clock, costOf, creditText, creditText1, mTokens, swapAmountText, usd } from './pricing';

describe('pricing', () => {
  it('prices the three token classes at 1000 / 3000 / 100 per million', () => {
    expect(RATE.prompt * 1e6).toBe(1000);
    expect(RATE.completion * 1e6).toBe(3000);
    expect(RATE.cached * 1e6).toBe(100);
  });

  it('anchors 1000 credits to one dollar', () => {
    expect(CREDITS_PER_USD).toBe(1000);
    expect(usd(1000)).toBe('$1.00');
    expect(usd(412_860)).toBe('$412.86');
  });

  it('does not charge cached tokens at the prompt rate', () => {
    const cheap = costOf({ prompt: 0, cached: 10_000, completion: 0 });
    const full = costOf({ prompt: 10_000, cached: 0, completion: 0 });
    expect(full / cheap).toBe(10);
  });

  it('charges a mixed request at each class rate exactly once', () => {
    // the worked example on the pricing screen
    expect(costOf({ prompt: 10_000, cached: 0, completion: 800 })).toBeCloseTo(12.4, 6);
    expect(costOf({ prompt: 0, cached: 10_000, completion: 800 })).toBeCloseTo(3.4, 6);
  });

  it('reports how far a balance goes in each token class', () => {
    // Past 100M the tenths stop carrying information, so they are dropped.
    expect(mTokens(412_860, RATE.prompt)).toBe('413M');
    expect(mTokens(412_860, RATE.completion)).toBe('138M');
    expect(mTokens(412_860, RATE.cached)).toBe('4129M');
  });

  it('keeps one decimal below the 100M threshold', () => {
    expect(mTokens(25_000, RATE.prompt)).toBe('25.0M');
    expect(mTokens(99_900, RATE.prompt)).toBe('99.9M');
    expect(mTokens(100_000, RATE.prompt)).toBe('100M');
  });

  it('formats elapsed time as mm:ss', () => {
    expect(clock(0)).toBe('00:00');
    expect(clock(96)).toBe('01:36');
    expect(clock(306)).toBe('05:06');
  });

  it('renders sub-credit amounts down to the wire\'s micro-credit resolution', () => {
    expect(creditText(0.0005)).toBe('0.0005');
    expect(creditText(0.0001)).toBe('0.0001');
    expect(creditText(0.0005)).not.toBe('0');
  });

  it('renders fractional amounts with trailing zeros trimmed', () => {
    expect(creditText(12.6444)).toBe('12.6444');
    expect(creditText(4.842)).toBe('4.842');
    expect(creditText(12.5)).toBe('12.5');
    expect(creditText(1.25)).toBe('1.25');
    // A float sum that lands on a clean value must not show float dust.
    expect(creditText(0.1 + 0.2)).toBe('0.3');
  });

  it('renders whole credit amounts unchanged, grouped', () => {
    expect(creditText(12)).toBe('12');
    expect(creditText(412_860)).toBe('412,860');
    expect(creditText(5_000)).toBe('5,000');
  });

  it('renders zero as zero and drops only what is below display precision', () => {
    expect(creditText(0)).toBe('0');
    expect(creditText(0.00004)).toBe('0');
  });

  describe('creditText1', () => {
    it('rounds down to one decimal, never up', () => {
      expect(creditText1(1.2999)).toBe('1.2');
      expect(creditText1(1.25)).toBe('1.2');
      expect(creditText1(0.15)).toBe('0.1');
    });

    it('does not show float multiplication artifacts', () => {
      expect(creditText1(0.7)).toBe('0.7');
    });

    it('keeps whole numbers with a trailing .0, grouped', () => {
      expect(creditText1(412_860)).toBe('412,860.0');
      expect(creditText1(5_000)).toBe('5,000.0');
    });

    it('floors sub-decimal amounts to zero, never overstating a balance', () => {
      expect(creditText1(0)).toBe('0.0');
      expect(creditText1(0.04)).toBe('0.0');
    });
  });

  describe('swapAmountText', () => {
    it('formats the swap input side at up to six decimals, trailing zeros trimmed', () => {
      expect(swapAmountText(3_321_000_000_000_000, 18)).toBe('0.003321');
      // A small ETH amount must never truncate to "0.00".
      expect(swapAmountText(1_000_000_000_000_000, 18)).toBe('0.001');
      expect(swapAmountText(1_010_000_000_000_000, 18)).toBe('0.00101');
    });

    it('drops trailing zeros on whole and short figures', () => {
      expect(swapAmountText(2 * 10 ** 18, 18)).toBe('2');
      expect(swapAmountText(15 * 10 ** 6, 6)).toBe('15');
      expect(swapAmountText(1_500_000, 6)).toBe('1.5');
    });
  });
});
