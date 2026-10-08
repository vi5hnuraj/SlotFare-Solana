import {
  Connection,
  PublicKey,
  Transaction,
} from '@solana/web3.js';
import {
  createAssociatedTokenAccountIdempotentInstruction,
  createTransferCheckedInstruction,
  getAssociatedTokenAddress,
} from '@solana/spl-token';

/** One `GET /console/deposit-target` payload, the subset the transfers
 *  need. Declared structurally so this module never imports the API layer. */
export interface TransferTarget {
  /** Public, keyless JSON-RPC endpoint for the cluster. */
  rpc: string;
  /** The USDC mint (base58). */
  mint: string;
  /** The mint's decimals; 6 on every USDC deployment. */
  decimals: number;
}

/** A failure the wallet or the cluster produced, classified for display.
 *  `cancelled` is true when the user rejected the prompt - the screens
 *  show that as a dismissible notice, never an error card. */
export class TransferError extends Error {
  readonly cancelled: boolean;

  constructor(message: string, opts?: { cancelled?: boolean }) {
    super(message);
    this.name = 'TransferError';
    this.cancelled = opts?.cancelled === true;
  }
}

/** Phantom's rejection (code 4001) or a reject-shaped message, at any
 *  depth of the cause chain. */
export function isUserRejection(err: unknown): boolean {
  const seen = new Set<unknown>();
  let cur: unknown = err;
  while (cur !== null && typeof cur === 'object' && !seen.has(cur)) {
    seen.add(cur);
    const e = cur as { code?: unknown; message?: unknown; cause?: unknown };
    if (e.code === 4001) return true;
    if (
      typeof e.message === 'string' &&
      /user rejected|denied|request rejected|canceled|cancelled/i.test(e.message)
    ) {
      return true;
    }
    cur = e.cause;
  }
  return false;
}

/** Whether a transfer failed because the payer's USDC balance (or its token
 *  account) cannot cover the amount - a fact about the wallet, not about
 *  the cluster. */
export function isInsufficientFunds(err: unknown): boolean {
  const msg = err instanceof Error ? err.message : String(err ?? '');
  return /insufficient (lamports|funds|tokens)|insufficient balance|custom program error: 0x1\b|attempt to debit an account but found no record of a prior credit/i.test(
    msg,
  );
}

/** Decode the console's base64 wire transaction. `atob` is the browser's own
 *  base64 decoder, so no Buffer polyfill is pulled in for it. */
export function base64ToBytes(b64: string): Uint8Array {
  const bin = atob(b64);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i += 1) out[i] = bin.charCodeAt(i);
  return out;
}

/** The market program's own rejections, as the cluster reports them: a
 *  custom program error reaches the client as `{"Custom":106}` inside an
 *  `InstructionError`, so both the decimal and the hex form are matched.
 *  106 ListingNotOpen, 108 SelfBuy (see programs/slotfare-ledger). */
const LISTING_NOT_OPEN =
  /("Custom":\s*(106|110)\b|Custom\((106|110)\)|custom program error: 0x6[ae])/i;
const SELF_BUY = /("Custom":\s*108\b|Custom\(108\)|custom program error: 0x6c)/i;

/** Whether the program rejected the transaction because the listing is no
 *  longer takeable - it was bought, withdrawn or already taken. */
export function isListingNotOpen(err: unknown): boolean {
  const msg = err instanceof Error ? err.message : String(err ?? '');
  return LISTING_NOT_OPEN.test(msg);
}

/** Whether the program rejected the transaction because the signer is the
 *  listing's own seller. The escrow refuses a self-sale on chain. */
export function isSelfBuy(err: unknown): boolean {
  const msg = err instanceof Error ? err.message : String(err ?? '');
  return SELF_BUY.test(msg);
}

/**
 * Sign and submit a transaction the console prepared for this wallet. The
 * daemon builds the market instruction and names the caller as fee payer,
 * but it never holds a user's key: collecting the signature is the browser's
 * job, and this is where it happens.
 *
 * Nothing here decides what the transaction does - the deployed program is
 * the authority for that - and a rejection comes back as the program's own
 * error, not as a local guess.
 *
 * Returns the transaction signature. Rejects with `TransferError`.
 */
export async function sendPreparedTransaction(opts: {
  /** Public, keyless JSON-RPC endpoint for the cluster. */
  rpc: string;
  /** base64 of the unsigned transaction from the console's prepare route. */
  preparedBase64: string;
  /** Ask the wallet to sign; wired to the wallet service by the caller. */
  sign: (tx: Transaction) => Promise<Transaction>;
}): Promise<string> {
  let tx: Transaction;
  try {
    tx = Transaction.from(base64ToBytes(opts.preparedBase64));
  } catch (err) {
    throw new TransferError(
      `That transaction could not be read: ${err instanceof Error ? err.message : String(err)}`,
    );
  }

  const connection = new Connection(opts.rpc, 'confirmed');

  let signed: Transaction;
  try {
    signed = await opts.sign(tx);
  } catch (err) {
    if (isUserRejection(err)) {
      throw new TransferError('The request was cancelled. Nothing was sent.', {
        cancelled: true,
      });
    }
    throw new TransferError(err instanceof Error ? err.message : String(err));
  }

  let signature: string;
  try {
    signature = await connection.sendRawTransaction(signed.serialize(), {
      skipPreflight: false,
    });
  } catch (err) {
    if (isInsufficientFunds(err)) {
      throw new TransferError('Your wallet does not hold enough SOL to pay for this transaction.');
    }
    throw new TransferError(err instanceof Error ? err.message : String(err));
  }

  try {
    const conf = await connection.confirmTransaction(signature, 'confirmed');
    if (conf.value.err !== null) {
      throw new TransferError(`The transaction failed on chain: ${JSON.stringify(conf.value.err)}`);
    }
  } catch (err) {
    if (err instanceof TransferError) throw err;
    throw new TransferError(
      `The transaction did not confirm: ${err instanceof Error ? err.message : String(err)}`,
    );
  }
  return signature;
}

/**
 * Send a checked token transfer from the connected wallet and wait for it
 * to confirm. The payer's public key is taken from `payerAddress` (the
 * session address); the prompt itself is answered by `sign`, which the
 * screens wire to the wallet service.
 *
 * `destination` is either a token account to credit directly (the treasury
 * on the deposit path) or an owner whose token account is derived - and
 * created idempotently when missing - which is what a marketplace seller
 * who never held USDC needs.
 *
 * Returns the transaction signature. Rejects with `TransferError`.
 */
export async function sendTokenTransfer(opts: {
  target: TransferTarget;
  /** Session address of the payer (base58). */
  payerAddress: string;
  /** Whole token units the transfer must net to the destination. */
  amountBase: bigint;
  /** Where the money goes. */
  destination: { kind: 'account'; address: string } | { kind: 'owner'; address: string };
  /** Ask the wallet to sign; wired to the wallet service by the caller. */
  sign: (tx: Transaction) => Promise<Transaction>;
}): Promise<string> {
  const { target } = opts;
  let payer: PublicKey;
  let dest: PublicKey;
  try {
    payer = new PublicKey(opts.payerAddress);
    const mint = new PublicKey(target.mint);
    const destOwner = new PublicKey(opts.destination.address);
    dest =
      opts.destination.kind === 'owner'
        ? await getAssociatedTokenAddress(mint, destOwner)
        : destOwner;
  } catch (err) {
    throw new TransferError(
      err instanceof Error ? err.message : 'Invalid wallet address',
    );
  }

  if (opts.amountBase <= 0n) {
    throw new TransferError('Transfer amount must be above zero');
  }

  const connection = new Connection(target.rpc, 'confirmed');
  const source = await getAssociatedTokenAddress(new PublicKey(target.mint), payer);

  const instructions = [];
  if (opts.destination.kind === 'owner') {
    // Idempotent: no-op when the seller's token account already exists.
    instructions.push(
      createAssociatedTokenAccountIdempotentInstruction(
        payer,
        dest,
        new PublicKey(opts.destination.address),
        new PublicKey(target.mint),
      ),
    );
  }
  instructions.push(
    createTransferCheckedInstruction(
      source,
      new PublicKey(target.mint),
      dest,
      payer,
      opts.amountBase,
      target.decimals,
    ),
  );

  let blockhash: string;
  let lastValidBlockHeight: number;
  try {
    ({ blockhash, lastValidBlockHeight } = await connection.getLatestBlockhash('confirmed'));
  } catch (err) {
    throw new TransferError(
      `Could not reach the cluster: ${err instanceof Error ? err.message : String(err)}`,
    );
  }

  const tx = new Transaction({ feePayer: payer, blockhash, lastValidBlockHeight });
  tx.add(...instructions);

  let signed: Transaction;
  try {
    signed = await opts.sign(tx);
  } catch (err) {
    if (isUserRejection(err)) {
      throw new TransferError('The request was cancelled. Nothing was sent.', {
        cancelled: true,
      });
    }
    throw new TransferError(err instanceof Error ? err.message : String(err));
  }

  let signature: string;
  try {
    signature = await connection.sendRawTransaction(signed.serialize(), {
      skipPreflight: false,
    });
  } catch (err) {
    if (isInsufficientFunds(err)) {
      throw new TransferError('Your wallet does not hold enough USDC for this amount.');
    }
    throw new TransferError(err instanceof Error ? err.message : String(err));
  }

  try {
    const conf = await connection.confirmTransaction(
      { signature, blockhash, lastValidBlockHeight },
      'confirmed',
    );
    if (conf.value.err !== null) {
      throw new TransferError(`The transfer failed on chain: ${JSON.stringify(conf.value.err)}`);
    }
  } catch (err) {
    if (err instanceof TransferError) throw err;
    if (isInsufficientFunds(err)) {
      throw new TransferError('Your wallet does not hold enough USDC for this amount.');
    }
    throw new TransferError(
      `The transfer did not confirm: ${err instanceof Error ? err.message : String(err)}`,
    );
  }
  return signature;
}
