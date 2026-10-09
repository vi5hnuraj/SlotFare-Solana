/**
 * The one status label for a listing row, from the viewer's side of the
 * trade: the buyer's sold row reads 'Bought', the seller's reads 'Sold'.
 * The transactions list and the trade detail view both derive their label
 * from here, so the two can never disagree.
 */
export function listingStatusLabel(
  role: 'buying' | 'selling',
  state: 'open' | 'cancelled' | 'sold',
  matured: boolean,
): string {
  if (role === 'selling') {
    if (state === 'sold') return 'Sold';
    if (state === 'cancelled') return 'Cancelled';
    return matured ? 'Matured' : 'For sale';
  }
  if (state === 'sold') return 'Bought';
  if (state === 'cancelled') return 'Cancelled';
  return 'Buying';
}
