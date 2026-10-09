import { listingPrice, shortAddress, usableSymbol } from './listing-price';

describe('usableSymbol', () => {
  it('returns a clean symbol trimmed of whitespace', () => {
    expect(usableSymbol('  USDC ')).toBe('USDC');
  });

  it('treats an empty or 0x-prefixed value as absent', () => {
    expect(usableSymbol('')).toBe('');
    expect(usableSymbol('   ')).toBe('');
    expect(usableSymbol('0x81A4C39b')).toBe('');
    expect(usableSymbol(undefined)).toBe('');
  });
});

describe('shortAddress', () => {
  it('renders a valid address in the standard 0x1234...abcd form', () => {
    expect(shortAddress('0x81A4C39b979FD6d71484b35F14799872593F3321')).toBe('0x81A4...3321');
  });

  it('renders a base58 wallet in the AbCd...wXyZ form', () => {
    expect(shortAddress('7xKXtg2CW87d97TXJSDpbD5jBkheTqA83TZRuJosgAsU')).toBe('7xKX...gAsU');
  });

  it('refuses anything that is not at least eight address characters', () => {
    expect(shortAddress('')).toBe('');
    expect(shortAddress('0x12')).toBe('');
    expect(shortAddress('deadbee0')).toBe('');
    expect(shortAddress('0x1234567g')).toBe('');
    expect(shortAddress(undefined)).toBe('');
  });
});

describe('listingPrice', () => {
  it('scales the raw wire amount by the wire decimals and labels with the symbol', () => {
    expect(listingPrice({ price: 5_000_000, token: 'USDC', tokenDecimals: 6 })).toEqual({
      text: '5.00',
      label: 'USDC',
    });
  });

  it('shows the raw marker and a shortened address for a fully unknown token', () => {
    const unknown = '0x1111222233334444555566667777888899990000';
    const parts = listingPrice({ price: 5_000_000, tokenAddress: unknown });
    expect(parts.text).toBe('5,000,000.00 raw');
    expect(parts.label).toBe('0x1111...0000');
  });

  it('never lets an address shadow a symbol', () => {
    const unknown = '0x1111222233334444555566667777888899990000';
    expect(
      listingPrice({ price: 100, token: 'USDC', tokenAddress: unknown, tokenDecimals: 6 }).label,
    ).toBe('USDC');
  });

  it('keeps the symbol label and the raw marker when the wire decimals are zero', () => {
    const unknown = '0x1111222233334444555566667777888899990000';
    const parts = listingPrice({
      price: 5_000_000,
      token: '0x81A4C39b',
      tokenAddress: unknown,
      tokenDecimals: 0,
    });
    expect(parts.text).toBe('5,000,000.00 raw');
    expect(parts.label).toBe('0x1111...0000');
    expect(parts.label).not.toContain('0x81A4C39b');
  });

  it('treats zero decimals as unknown even when a real-looking symbol is present', () => {
    const unknown = '0x1111222233334444555566667777888899990000';
    const parts = listingPrice({
      price: 5_000_000,
      token: 'USDC',
      tokenAddress: unknown,
      tokenDecimals: 0,
    });
    expect(parts.text).toBe('5,000,000.00 raw');
    expect(parts.label).toBe('USDC');
  });

  it('uses the em dash when neither symbol nor address is available', () => {
    const parts = listingPrice({ price: 5_000_000 });
    expect(parts.label).toBe('-');
    expect(parts.text).toBe('5,000,000.00 raw');
  });
});
