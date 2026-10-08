import { computed, signal } from '@angular/core';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { Router } from '@angular/router';
import { ConsoleStore } from '../core/console-store';
import { ThemeService } from '../core/theme';
import { ConsoleSession, WalletService } from '../core/wallet';
import { Wallet } from '../core/models';
import { ConnectComponent } from './connect.component';

/** A fixed data-URI icon, standing in for a provider's own image. */
const ICON =
  'data:image/svg+xml;utf8,<svg xmlns=%22http://www.w3.org/2000/svg%22 width=%2232%22 height=%2232%22></svg>';

function makeFakeWallet(): WalletService {
  const address = signal<string | null>(null);
  const chainId = signal<string | null>(null);
  const connecting = signal(false);
  const error = signal<string | null>(null);
  const accountId = signal<string | null>(null);
  const token = signal<string | null>(null);
  const session = computed<ConsoleSession | null>(() => {
    const t = token();
    if (t === null) return null;
    const a = address();
    const id = accountId();
    if (a === null || id === null) return null;
    return { address: a, accountId: id, token: t };
  });
  return {
    providers: signal<Wallet[]>([]),
    address,
    chainId,
    connecting,
    error,
    accountId,
    token,
    session,
    provider: null,
    connect: () => undefined,
    startSession: () => undefined,
    clearSession: () => undefined,
    dismissError: () => undefined,
  } as unknown as WalletService;
}

describe('ConnectComponent', () => {
  let fixture: ComponentFixture<ConnectComponent>;
  let fakeWallet: WalletService;

  function setup(providers: Wallet[]): void {
    fakeWallet = makeFakeWallet();
    fakeWallet.providers.set(providers);
    TestBed.configureTestingModule({
      imports: [ConnectComponent],
      providers: [
        { provide: Router, useValue: { navigate: () => Promise.resolve(true) } },
        {
          provide: ConsoleStore,
          useValue: { signedIn: () => false, connect: () => {} },
        },
        { provide: ThemeService, useValue: { theme: signal('light') } },
        { provide: WalletService, useValue: fakeWallet },
      ],
    });
    fixture = TestBed.createComponent(ConnectComponent);
    fixture.detectChanges();
  }

  it('renders the announced provider icon in place of the letter avatar', () => {
    setup([{ name: 'MetaMask', rdns: 'io.metamask', initial: 'M', icon: ICON }]);

    const img = fixture.nativeElement.querySelector('img.avatar') as HTMLImageElement;
    expect(img).toBeTruthy();
    expect(img.src).toBe(ICON);
    // The letter avatar is gone for this wallet.
    expect(fixture.nativeElement.querySelector('span.avatar')).toBeNull();
  });

  it('falls back to the letter avatar when no icon was announced', () => {
    setup([{ name: 'MetaMask', rdns: 'io.metamask', initial: 'M' }]);

    const span = fixture.nativeElement.querySelector('span.avatar');
    expect(span).toBeTruthy();
    expect(span.textContent).toContain('M');
    expect(fixture.nativeElement.querySelector('img.avatar')).toBeNull();
  });

  it('falls back to the letter avatar when the icon fails to load', () => {
    setup([{ name: 'MetaMask', rdns: 'io.metamask', initial: 'M', icon: ICON }]);
    const img = fixture.nativeElement.querySelector('img.avatar')!;
    img.dispatchEvent(new Event('error'));
    fixture.detectChanges();

    expect(fixture.nativeElement.querySelector('img.avatar')).toBeNull();
    const span = fixture.nativeElement.querySelector('span.avatar');
    expect(span).toBeTruthy();
    expect(span.textContent).toContain('M');
  });

  it('pluralizes the provider count in the header', () => {
    setup([
      { name: 'MetaMask', rdns: 'io.metamask', initial: 'M' },
      { name: 'Rabby', rdns: 'io.rabby', initial: 'R' },
    ]);
    let eyebrow = fixture.nativeElement.querySelector('.eyebrow') as HTMLElement;
    expect(eyebrow.textContent?.trim()).toMatch(/2 providers$/);

    fakeWallet.providers.set([
      { name: 'MetaMask', rdns: 'io.metamask', initial: 'M' },
    ]);
    fixture.detectChanges();
    eyebrow = fixture.nativeElement.querySelector('.eyebrow') as HTMLElement;
    expect(eyebrow.textContent?.trim()).toMatch(/1 provider$/);
  });
});
