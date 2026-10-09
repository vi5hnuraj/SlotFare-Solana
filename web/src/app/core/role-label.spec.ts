import { listingStatusLabel } from './role-label';

describe('listingStatusLabel', () => {
  it('labels a sold selling row Sold, from the seller side', () => {
    expect(listingStatusLabel('selling', 'sold', true)).toBe('Sold');
  });

  it('labels a cancelled selling row Cancelled', () => {
    expect(listingStatusLabel('selling', 'cancelled', true)).toBe('Cancelled');
  });

  it('labels an open matured selling row Matured', () => {
    expect(listingStatusLabel('selling', 'open', true)).toBe('Matured');
  });

  it('labels an open immature selling row For sale', () => {
    expect(listingStatusLabel('selling', 'open', false)).toBe('For sale');
  });

  it('labels a sold buying row Bought, not Sold', () => {
    expect(listingStatusLabel('buying', 'sold', true)).toBe('Bought');
  });

  it('labels a cancelled buying row Cancelled', () => {
    expect(listingStatusLabel('buying', 'cancelled', true)).toBe('Cancelled');
  });

  it('labels an open buying row Buying', () => {
    expect(listingStatusLabel('buying', 'open', false)).toBe('Buying');
  });
});
