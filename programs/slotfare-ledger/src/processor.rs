//! The on-chain entrypoint: authority checks, PDA derivation, and the thin
//! glue that binds an instruction to [`CreditAccount`]'s tested transitions.
//!
//! The division of labour is deliberate. [`crate::state`] owns every rule that
//! decides whether a transition is legal, and it is tested without a Solana
//! runtime. This file owns the rules that depend on *who is calling*: that only
//! the configured authority may mint credits or commit a usage root, that an
//! account really is the PDA it claims to be, and that the program owns the
//! bytes it is about to rewrite. Authority is the part the original enforced
//! inside its contracts, so this is the file that restores the original's trust
//! model rather than approximating it.
//!
//! ## Why the checks here matter
//!
//! * **Account ownership.** A credit account passed as writable must be owned
//!   by this program. Without that check an attacker passes a look-alike account
//!   they control, and the program would happily write balances into it.
//! * **PDA verification.** Every credit account must equal
//!   `[b"credit", owner]`. Otherwise the caller could present an account whose
//!   `owner` account is a stranger's and move balances that are not theirs.
//!   Verification recomputes the canonical bump, so a non-canonical account
//!   created by some other means is rejected too.
//! * **Signers.** Minting and settling are restricted to the config authority,
//!   which must have signed. Locking, unlocking and transferring require the
//!   owning account to have signed.
//! * **Config binding.** The single config account binds one authority at
//!   deployment. Whoever calls `InitConfig` first installs themselves, so the
//!   runbook initialises the program in the same flow as the deploy
//!   (docs/onchain-ledger.md).

// The `entrypoint!` macro probes `cfg(feature = "custom-heap")` and
// `cfg(target_os = "solana")` from inside our crate, so the modern compiler
// reports them as unexpected here. They are the macro's, not ours; the allow
// keeps the build output readable without touching a single lint we own.
#![allow(unexpected_cfgs)]

use solana_program::{
    account_info::{next_account_info, AccountInfo},
    entrypoint,
    entrypoint::ProgramResult,
    instruction::{AccountMeta, Instruction as SolInstruction},
    msg,
    program::{invoke, invoke_signed},
    program_error::ProgramError,
    pubkey::Pubkey,
    rent::Rent,
    sysvar::Sysvar,
};
use solana_system_interface::{instruction as system_instruction, program as system_program};

use crate::instruction::Instruction;
use crate::state::{CreditAccount, LedgerError, ListingAccount, ACCOUNT_SIZE, LISTING_SIZE};

/// PDA seed for the program's single config account.
pub const CONFIG_SEED: &[u8] = b"config";
/// PDA seed for a per-owner credit account: `[CREDIT_SEED, owner]`.
pub const CREDIT_SEED: &[u8] = b"credit";
/// PDA seed for a market listing: `[LISTING_SEED, seller, nonce]`.
pub const LISTING_SEED: &[u8] = b"listing";
/// PDA seed for a listing's escrow credit account: `[ESCROW_SEED, listing]`.
pub const ESCROW_SEED: &[u8] = b"escrow";

/// The SPL Token program, whose `TransferChecked` the buy instruction calls.
/// Encoded as bytes, not parsed, so the crate keeps no base58 dependency.
pub const TOKEN_PROGRAM_ID: [u8; 32] = [
    0x06, 0xdd, 0xf6, 0xe1, 0xd7, 0x65, 0xa1, 0x93, 0xd9, 0xcb, 0xe1, 0x46, 0xce, 0xeb, 0x79, 0xac,
    0x1c, 0xb4, 0x85, 0xed, 0x5f, 0x5b, 0x37, 0x91, 0x3a, 0x8c, 0xf5, 0x85, 0x7e, 0xff, 0x00, 0xa9,
];

/// The `TransferChecked` instruction tag in the SPL Token program.
const SPL_TRANSFER_CHECKED: u8 = 12;
/// Offset of the `decimals` byte in an SPL Token mint account.
const SPL_MINT_DECIMALS_OFFSET: usize = 44;
/// Minimum length of an SPL Token mint account.
const SPL_MINT_SIZE: usize = 82;
/// Offset of the `mint` field in an SPL Token account; the owner follows it.
const SPL_TOKEN_MINT_OFFSET: usize = 0;
/// Minimum length of an SPL Token account (mint + owner + amount).
const SPL_TOKEN_SIZE: usize = 72;

/// Config layout: magic(4) authority(32) usdc_mint(32) treasury(32).
pub const CONFIG_SIZE: usize = 100;
/// ASCII tag so a wrong account is a decode error, not a plausible read.
pub const CONFIG_MAGIC: [u8; 4] = *b"SFCG";

/// Program-specific error codes, stable on the wire. A client maps these back
/// to a human sentence; changing one would change what an existing client
/// reports.
pub const ERR_NOT_AUTHORITY: u32 = 100;
pub const ERR_BAD_PDA: u32 = 101;
pub const ERR_CONFIG_NOT_INITIALISED: u32 = 102;
pub const ERR_ACCOUNT_NOT_OWNED: u32 = 103;
pub const ERR_BAD_ACCOUNT_SIZE: u32 = 104;
pub const ERR_CONFIG_ALREADY_INITIALISED: u32 = 105;
pub const ERR_LISTING_NOT_OPEN: u32 = 106;
pub const ERR_NOT_LISTING_SELLER: u32 = 107;
pub const ERR_SELF_BUY: u32 = 108;
pub const ERR_BAD_LISTING_TERMS: u32 = 109;
pub const ERR_LISTING_ALREADY_OPEN: u32 = 110;
pub const ERR_BAD_TOKEN_PROGRAM: u32 = 111;
pub const ERR_BAD_TOKEN_ACCOUNT: u32 = 112;
pub const ERR_MINT_MISMATCH: u32 = 113;

/// Map a state-machine rejection onto the wire. The codes are the enum's
/// declaration order, so they are stable as long as the enum is appended to.
impl From<LedgerError> for ProgramError {
    fn from(e: LedgerError) -> Self {
        ProgramError::Custom(match e {
            LedgerError::InsufficientSpendable => 1,
            LedgerError::InsufficientLocked => 2,
            LedgerError::RootAlreadyCommitted => 3,
            LedgerError::UsageIdsNotMonotonic => 4,
            LedgerError::ZeroCreditsSettle => 5,
            LedgerError::Overflow => 6,
            LedgerError::BadAccountData => 7,
            LedgerError::ListingNotOpen => 8,
        })
    }
}

fn custom(code: u32) -> ProgramError {
    ProgramError::Custom(code)
}

/// The single config account: `[CONFIG_SEED]`.
pub fn config_pda(program_id: &Pubkey) -> (Pubkey, u8) {
    Pubkey::find_program_address(&[CONFIG_SEED], program_id)
}

/// One owner's credit account: `[CREDIT_SEED, owner]`.
pub fn credit_pda(program_id: &Pubkey, owner: &Pubkey) -> (Pubkey, u8) {
    Pubkey::find_program_address(&[CREDIT_SEED, owner.as_ref()], program_id)
}

/// A market listing: `[LISTING_SEED, seller, nonce]`.
pub fn listing_pda(program_id: &Pubkey, seller: &Pubkey, nonce: u64) -> (Pubkey, u8) {
    Pubkey::find_program_address(
        &[LISTING_SEED, seller.as_ref(), &nonce.to_le_bytes()],
        program_id,
    )
}

/// A listing's escrow credit account: `[ESCROW_SEED, listing]`.
pub fn escrow_pda(program_id: &Pubkey, listing: &Pubkey) -> (Pubkey, u8) {
    Pubkey::find_program_address(&[ESCROW_SEED, listing.as_ref()], program_id)
}

entrypoint!(process_instruction);

/// Dispatch. Anything not recognised is rejected by `Instruction::decode`
/// before a single account is touched.
pub fn process_instruction(
    program_id: &Pubkey,
    accounts: &[AccountInfo],
    data: &[u8],
) -> ProgramResult {
    let ix = Instruction::decode(data)?;
    match ix {
        Instruction::InitConfig {
            authority,
            usdc_mint,
            treasury,
        } => init_config(program_id, accounts, authority, usdc_mint, treasury),
        Instruction::InitCredit => init_credit(program_id, accounts),
        Instruction::Credit { amount } => credit(program_id, accounts, amount),
        Instruction::Lock { amount } => lock(program_id, accounts, amount),
        Instruction::Unlock { amount } => unlock(program_id, accounts, amount),
        Instruction::CommitAndSettle {
            root,
            first_usage_id,
            last_usage_id,
            credits,
        } => commit_and_settle(
            program_id,
            accounts,
            root,
            first_usage_id,
            last_usage_id,
            credits,
        ),
        Instruction::Transfer { amount } => transfer(program_id, accounts, amount),
        Instruction::List {
            credits,
            price,
            nonce,
            pay_mint,
        } => list(program_id, accounts, credits, price, nonce, pay_mint),
        Instruction::Buy => buy(program_id, accounts),
        Instruction::CancelList => cancel_list(program_id, accounts),
    }
}

/// `InitConfig` — accounts: `[authority (signer, writable), config, system]`.
///
/// The signer must be the authority it names, so nobody can install a
/// third party as the program's authority. The first caller still wins, which
/// is why deployment and initialisation are one flow.
fn init_config(
    program_id: &Pubkey,
    accounts: &[AccountInfo],
    authority: [u8; 32],
    usdc_mint: [u8; 32],
    treasury: [u8; 32],
) -> ProgramResult {
    let it = &mut accounts.iter();
    let payer = next_account_info(it)?;
    let config = next_account_info(it)?;
    let system_program = next_account_info(it)?;

    require_signer(payer)?;
    if payer.key.to_bytes() != authority {
        return Err(custom(ERR_NOT_AUTHORITY));
    }
    let (expected, bump) = config_pda(program_id);
    if config.key != &expected {
        return Err(custom(ERR_BAD_PDA));
    }
    require_system_program(system_program)?;

    // InitConfig is first-caller-wins, so a second call is a normal thing for
    // an operator to attempt (re-running deployment, or racing a config that
    // has landed but is not visible to their RPC yet). Say so plainly instead
    // of letting the `create_account` CPI fail with a bare system error that
    // reads as a mystery: the caller can then read the config and continue.
    if config.lamports() != 0 || !config.data_is_empty() {
        return Err(custom(ERR_CONFIG_ALREADY_INITIALISED));
    }

    let rent = Rent::get().unwrap_or_default();
    let create = system_instruction::create_account(
        payer.key,
        config.key,
        rent.minimum_balance(CONFIG_SIZE),
        CONFIG_SIZE as u64,
        program_id,
    );
    let bump_slice = [bump];
    let seeds: &[&[u8]] = &[CONFIG_SEED, &bump_slice];
    invoke_signed(
        &create,
        &[payer.clone(), config.clone(), system_program.clone()],
        &[seeds],
    )?;

    let mut bytes = [0u8; CONFIG_SIZE];
    bytes[0..4].copy_from_slice(&CONFIG_MAGIC);
    bytes[4..36].copy_from_slice(&authority);
    bytes[36..68].copy_from_slice(&usdc_mint);
    bytes[68..100].copy_from_slice(&treasury);
    write_all(config, &bytes)?;

    msg!("slotfare: config initialised, authority {}", payer.key);
    Ok(())
}

/// `InitCredit` — accounts: `[payer (signer, writable), owner, credit, system]`.
/// Creates the owner's empty credit account. Creating an account is harmless,
/// so this needs no authority beyond the lamports to pay rent.
fn init_credit(program_id: &Pubkey, accounts: &[AccountInfo]) -> ProgramResult {
    let it = &mut accounts.iter();
    let payer = next_account_info(it)?;
    let owner = next_account_info(it)?;
    let credit = next_account_info(it)?;
    let system_program = next_account_info(it)?;

    require_signer(payer)?;
    require_credit_pda(program_id, owner.key, credit.key)?;
    require_system_program(system_program)?;

    let rent = Rent::get().unwrap_or_default();
    let create = system_instruction::create_account(
        payer.key,
        credit.key,
        rent.minimum_balance(ACCOUNT_SIZE),
        ACCOUNT_SIZE as u64,
        program_id,
    );
    let (_, bump) = credit_pda(program_id, owner.key);
    let bump_slice = [bump];
    let seeds: &[&[u8]] = &[CREDIT_SEED, owner.key.as_ref(), &bump_slice];
    invoke_signed(
        &create,
        &[payer.clone(), credit.clone(), system_program.clone()],
        &[seeds],
    )?;

    write_all(credit, &CreditAccount::default().encode())?;

    msg!("slotfare: credit account opened for {}", owner.key);
    Ok(())
}

/// `Credit` — accounts: `[authority (signer), config, owner, credit]`.
/// The deposit rail's mint. Restricted to the config authority: the chain
/// cannot see the deposit, so it trusts the rail's key exactly as far as the
/// original trusted its deposit contract's role.
fn credit(program_id: &Pubkey, accounts: &[AccountInfo], amount: u64) -> ProgramResult {
    let it = &mut accounts.iter();
    let authority = next_account_info(it)?;
    let config = next_account_info(it)?;
    let owner = next_account_info(it)?;
    let credit = next_account_info(it)?;

    require_authority(program_id, authority, config)?;
    require_credit_pda(program_id, owner.key, credit.key)?;
    with_credit(program_id, credit, |a| a.credit(amount))?;
    msg!("slotfare: minted {} credits for {}", amount, owner.key);
    Ok(())
}

/// `Lock` — accounts: `[owner (signer), credit]`. Lists credits for sale by
/// making them unspendable.
fn lock(program_id: &Pubkey, accounts: &[AccountInfo], amount: u64) -> ProgramResult {
    let it = &mut accounts.iter();
    let owner = next_account_info(it)?;
    let credit = next_account_info(it)?;

    require_signer(owner)?;
    require_credit_pda(program_id, owner.key, credit.key)?;
    with_credit(program_id, credit, |a| a.lock(amount))
}

/// `Unlock` — accounts: `[owner (signer), credit]`. Cancels a listing.
fn unlock(program_id: &Pubkey, accounts: &[AccountInfo], amount: u64) -> ProgramResult {
    let it = &mut accounts.iter();
    let owner = next_account_info(it)?;
    let credit = next_account_info(it)?;

    require_signer(owner)?;
    require_credit_pda(program_id, owner.key, credit.key)?;
    with_credit(program_id, credit, |a| a.unlock(amount))
}

/// `CommitAndSettle` — accounts: `[authority (signer), config, owner, credit]`.
///
/// Atomic instruction for `CommitAndSettle`: the usage root and the burn it covers
/// are applied in one atomic transition, so there is no burn without a committed root
/// and one root per burn. The settler must be the configured authority.
fn commit_and_settle(
    program_id: &Pubkey,
    accounts: &[AccountInfo],
    root: [u8; 32],
    first_usage_id: u64,
    last_usage_id: u64,
    credits: u64,
) -> ProgramResult {
    let it = &mut accounts.iter();
    let authority = next_account_info(it)?;
    let config = next_account_info(it)?;
    let owner = next_account_info(it)?;
    let credit = next_account_info(it)?;

    require_authority(program_id, authority, config)?;
    require_credit_pda(program_id, owner.key, credit.key)?;
    with_credit(program_id, credit, |a| {
        a.commit_and_settle(root, first_usage_id, last_usage_id, credits)
    })?;
    msg!(
        "slotfare: settled {} credits for {} over usage {}..={}",
        credits,
        owner.key,
        first_usage_id,
        last_usage_id
    );
    Ok(())
}

/// `Transfer` — accounts: `[from_owner (signer), from_credit, to_owner,
/// to_credit]`. Moves spendable credits in one instruction: the market sale.
/// The sender signs, and both sides must be genuine PDAs of the named owners,
/// so credits cannot be pushed into someone else's account without them holding
/// it, nor pulled out of an account the signer does not own.
fn transfer(program_id: &Pubkey, accounts: &[AccountInfo], amount: u64) -> ProgramResult {
    let it = &mut accounts.iter();
    let from_owner = next_account_info(it)?;
    let from_credit = next_account_info(it)?;
    let to_owner = next_account_info(it)?;
    let to_credit = next_account_info(it)?;

    require_signer(from_owner)?;
    require_credit_pda(program_id, from_owner.key, from_credit.key)?;
    require_credit_pda(program_id, to_owner.key, to_credit.key)?;
    if from_credit.key == to_credit.key {
        return Err(custom(ERR_BAD_PDA));
    }
    // Debit first: if the sender is short the whole transaction reverts and the
    // receiver is never credited, so the two sides cannot diverge.
    with_credit(program_id, from_credit, |a| a.transfer_out(amount))?;
    with_credit(program_id, to_credit, |a| a.receive(amount))?;
    msg!(
        "slotfare: transferred {} credits to {}",
        amount,
        to_owner.key
    );
    Ok(())
}

/// `List` — accounts: `[seller (signer, writable), seller_credit, listing,
/// escrow, system]`. Opens a market listing by moving `credits` out of the
/// seller's spendable balance into a program-owned escrow, so a listed credit
/// is unspendable by the seller for as long as the listing is open.
///
/// This is the original `CreditMarket.list` on Solana, and it is why the
/// market can settle without the seller being online: the credits are already
/// out of the seller's hands, held by the program against the listing terms.
fn list(
    program_id: &Pubkey,
    accounts: &[AccountInfo],
    credits: u64,
    price: u64,
    nonce: u64,
    pay_mint: [u8; 32],
) -> ProgramResult {
    let it = &mut accounts.iter();
    let seller = next_account_info(it)?;
    let seller_credit = next_account_info(it)?;
    let listing = next_account_info(it)?;
    let escrow = next_account_info(it)?;
    let system_program = next_account_info(it)?;

    require_signer(seller)?;
    require_credit_pda(program_id, seller.key, seller_credit.key)?;
    if credits == 0 || price == 0 || pay_mint == [0u8; 32] {
        return Err(custom(ERR_BAD_LISTING_TERMS));
    }
    let (expected_listing, lbump) = listing_pda(program_id, seller.key, nonce);
    if listing.key != &expected_listing {
        return Err(custom(ERR_BAD_PDA));
    }
    let (expected_escrow, ebump) = escrow_pda(program_id, listing.key);
    if escrow.key != &expected_escrow {
        return Err(custom(ERR_BAD_PDA));
    }
    require_system_program(system_program)?;
    // A listing opens once. Both accounts must be fresh, so a re-run cannot
    // overwrite terms under an escrow that already holds credits.
    if listing.lamports() != 0 || !listing.data_is_empty() {
        return Err(custom(ERR_LISTING_ALREADY_OPEN));
    }
    if escrow.lamports() != 0 || !escrow.data_is_empty() {
        return Err(custom(ERR_LISTING_ALREADY_OPEN));
    }

    let rent = Rent::get().unwrap_or_default();
    let nonce_bytes = nonce.to_le_bytes();

    let create_listing = system_instruction::create_account(
        seller.key,
        listing.key,
        rent.minimum_balance(LISTING_SIZE),
        LISTING_SIZE as u64,
        program_id,
    );
    let lbump_slice = [lbump];
    let lseeds: &[&[u8]] = &[
        LISTING_SEED,
        seller.key.as_ref(),
        &nonce_bytes,
        &lbump_slice,
    ];
    invoke_signed(
        &create_listing,
        &[seller.clone(), listing.clone(), system_program.clone()],
        &[lseeds],
    )?;

    let create_escrow = system_instruction::create_account(
        seller.key,
        escrow.key,
        rent.minimum_balance(ACCOUNT_SIZE),
        ACCOUNT_SIZE as u64,
        program_id,
    );
    let ebump_slice = [ebump];
    let eseeds: &[&[u8]] = &[ESCROW_SEED, listing.key.as_ref(), &ebump_slice];
    invoke_signed(
        &create_escrow,
        &[seller.clone(), escrow.clone(), system_program.clone()],
        &[eseeds],
    )?;

    let l = ListingAccount {
        seller: seller.key.to_bytes(),
        buyer: [0u8; 32],
        pay_mint,
        credits,
        price,
        nonce,
        state: crate::state::ListingState::Open,
    };
    write_all(listing, &l.encode())?;
    // The escrow starts empty, then receives exactly the listed credits. Both
    // moves are checked transitions, so the escrow can never hold a figure the
    // seller did not give up.
    write_all(escrow, &CreditAccount::default().encode())?;
    with_credit(program_id, seller_credit, |a| a.transfer_out(credits))?;
    with_credit(program_id, escrow, |a| a.receive(credits))?;

    msg!(
        "slotfare: listed {} credits at {} for seller {}",
        credits,
        price,
        seller.key
    );
    Ok(())
}

/// `Buy` — accounts: `[buyer (signer, writable), buyer_credit, seller_credit,
/// listing, escrow, buyer_token, seller_token, mint, token_program]`.
///
/// The original `CreditMarket.buy`, and the reason the market is atomic on
/// Solana: in one transition the buyer's USDC moves to the seller through an
/// SPL `TransferChecked` CPI, and the escrowed credits move to the buyer. If
/// either half fails the whole transaction reverts, so there is no state in
/// which the buyer paid and did not get credits, or got credits without
/// paying. The buyer signs; the seller need not be online.
fn buy(program_id: &Pubkey, accounts: &[AccountInfo]) -> ProgramResult {
    let it = &mut accounts.iter();
    let buyer = next_account_info(it)?;
    let buyer_credit = next_account_info(it)?;
    let seller_credit = next_account_info(it)?;
    let listing = next_account_info(it)?;
    let escrow = next_account_info(it)?;
    let buyer_token = next_account_info(it)?;
    let seller_token = next_account_info(it)?;
    let mint = next_account_info(it)?;
    let token_program = next_account_info(it)?;

    require_signer(buyer)?;
    // Reading the listing verifies it is the canonical PDA of its own seller
    // and nonce, so the terms cannot be swapped for a look-alike account.
    let l = read_listing(program_id, listing)?;
    if !l.is_open() {
        return Err(custom(ERR_LISTING_NOT_OPEN));
    }
    let seller_key = Pubkey::new_from_array(l.seller);
    if buyer.key == &seller_key {
        return Err(custom(ERR_SELF_BUY));
    }
    require_credit_pda(program_id, &seller_key, seller_credit.key)?;
    require_credit_pda(program_id, buyer.key, buyer_credit.key)?;
    require_escrow_pda(program_id, listing.key, escrow.key)?;
    require_token_program(token_program)?;
    if mint.key.to_bytes() != l.pay_mint {
        return Err(custom(ERR_MINT_MISMATCH));
    }
    require_token_account(buyer_token, &l.pay_mint)?;
    require_token_account(seller_token, &l.pay_mint)?;
    let decimals = read_mint_decimals(mint)?;

    // Half one: the buyer pays the seller. The buyer signed, so the CPI needs
    // no PDA authorization.
    let pay = transfer_checked_ix(
        buyer_token.key,
        mint.key,
        seller_token.key,
        buyer.key,
        l.price,
        decimals,
    );
    invoke(
        &pay,
        &[
            buyer_token.clone(),
            mint.clone(),
            seller_token.clone(),
            buyer.clone(),
            token_program.clone(),
        ],
    )?;

    // Half two: the escrow releases the credits to the buyer. The escrow is
    // program-owned, so this needs no seller signature - which is the whole
    // point of escrowing at list time.
    with_credit(program_id, escrow, |a| a.transfer_out(l.credits))?;
    with_credit(program_id, buyer_credit, |a| a.receive(l.credits))?;
    with_listing(program_id, listing, |acc| {
        acc.mark_sold(buyer.key.to_bytes())
    })?;

    msg!(
        "slotfare: sold {} credits to {} for {} units",
        l.credits,
        buyer.key,
        l.price
    );
    Ok(())
}

/// `CancelList` — accounts: `[seller (signer), listing, escrow, seller_credit]`.
/// Withdraws an open listing, returning the escrow to the seller's spendable
/// balance. Only the listing's own seller may cancel, and only while it is
/// open, so a cancelled sale cannot be withdrawn twice.
fn cancel_list(program_id: &Pubkey, accounts: &[AccountInfo]) -> ProgramResult {
    let it = &mut accounts.iter();
    let seller = next_account_info(it)?;
    let listing = next_account_info(it)?;
    let escrow = next_account_info(it)?;
    let seller_credit = next_account_info(it)?;

    require_signer(seller)?;
    let l = read_listing(program_id, listing)?;
    if !l.is_open() {
        return Err(custom(ERR_LISTING_NOT_OPEN));
    }
    if seller.key.to_bytes() != l.seller {
        return Err(custom(ERR_NOT_LISTING_SELLER));
    }
    require_escrow_pda(program_id, listing.key, escrow.key)?;
    require_credit_pda(program_id, seller.key, seller_credit.key)?;

    with_credit(program_id, escrow, |a| a.transfer_out(l.credits))?;
    with_credit(program_id, seller_credit, |a| a.receive(l.credits))?;
    with_listing(program_id, listing, |acc| acc.mark_cancelled())?;

    msg!("slotfare: cancelled listing of {} credits", l.credits);
    Ok(())
}

/// The `TransferChecked` instruction, encoded by hand so the crate needs no
/// SPL Token dependency: tag(1) amount(8) decimals(1), with accounts
/// `[source, mint, destination, authority]`.
fn transfer_checked_ix(
    source: &Pubkey,
    mint: &Pubkey,
    destination: &Pubkey,
    authority: &Pubkey,
    amount: u64,
    decimals: u8,
) -> SolInstruction {
    let mut data = Vec::with_capacity(10);
    data.push(SPL_TRANSFER_CHECKED);
    data.extend_from_slice(&amount.to_le_bytes());
    data.push(decimals);
    SolInstruction {
        program_id: Pubkey::new_from_array(TOKEN_PROGRAM_ID),
        accounts: vec![
            AccountMeta::new(*source, false),
            AccountMeta::new_readonly(*mint, false),
            AccountMeta::new(*destination, false),
            AccountMeta::new_readonly(*authority, true),
        ],
        data,
    }
}

/// Read a listing and verify it is the canonical PDA of the seller and nonce
/// its own bytes name. Without the derivation check the program would accept a
/// program-owned look-alike account with different terms.
fn read_listing(
    program_id: &Pubkey,
    listing: &AccountInfo,
) -> Result<ListingAccount, ProgramError> {
    require_owned(program_id, listing)?;
    let data = listing.try_borrow_data()?;
    if data.len() < LISTING_SIZE {
        return Err(custom(ERR_BAD_ACCOUNT_SIZE));
    }
    let decoded = ListingAccount::decode(&data[..])?;
    let seller = Pubkey::new_from_array(decoded.seller);
    if &listing_pda(program_id, &seller, decoded.nonce).0 != listing.key {
        return Err(custom(ERR_BAD_PDA));
    }
    Ok(decoded)
}

/// Load, transition and store one listing account, mirroring [`with_credit`].
fn with_listing<F>(program_id: &Pubkey, listing: &AccountInfo, f: F) -> ProgramResult
where
    F: FnOnce(&mut ListingAccount) -> Result<(), LedgerError>,
{
    require_owned(program_id, listing)?;
    let mut data = listing.try_borrow_mut_data()?;
    if data.len() < LISTING_SIZE {
        return Err(custom(ERR_BAD_ACCOUNT_SIZE));
    }
    let mut account = ListingAccount::decode(&data[..])?;
    f(&mut account)?;
    data[..LISTING_SIZE].copy_from_slice(&account.encode());
    Ok(())
}

fn require_escrow_pda(program_id: &Pubkey, listing: &Pubkey, escrow: &Pubkey) -> ProgramResult {
    if &escrow_pda(program_id, listing).0 != escrow {
        return Err(custom(ERR_BAD_PDA));
    }
    Ok(())
}

fn require_token_program(account: &AccountInfo) -> ProgramResult {
    if account.key.to_bytes() != TOKEN_PROGRAM_ID {
        return Err(custom(ERR_BAD_TOKEN_PROGRAM));
    }
    Ok(())
}

/// The account must be an SPL Token account of exactly `expected_mint`, owned
/// by the token program. Without the mint check a buyer could pay in a
/// worthless token that happens to share the account list shape.
fn require_token_account(account: &AccountInfo, expected_mint: &[u8; 32]) -> ProgramResult {
    if account.owner.to_bytes() != TOKEN_PROGRAM_ID {
        return Err(custom(ERR_BAD_TOKEN_ACCOUNT));
    }
    let data = account.try_borrow_data()?;
    if data.len() < SPL_TOKEN_SIZE {
        return Err(custom(ERR_BAD_TOKEN_ACCOUNT));
    }
    let start = SPL_TOKEN_MINT_OFFSET;
    if &data[start..start + 32] != expected_mint {
        return Err(custom(ERR_MINT_MISMATCH));
    }
    Ok(())
}

/// Read the mint's decimals, so `TransferChecked` is exact rather than guessed.
fn read_mint_decimals(mint: &AccountInfo) -> Result<u8, ProgramError> {
    if mint.owner.to_bytes() != TOKEN_PROGRAM_ID {
        return Err(custom(ERR_BAD_TOKEN_ACCOUNT));
    }
    let data = mint.try_borrow_data()?;
    if data.len() < SPL_MINT_SIZE {
        return Err(custom(ERR_BAD_TOKEN_ACCOUNT));
    }
    Ok(data[SPL_MINT_DECIMALS_OFFSET])
}

/// Load, transition and store one credit account. Every state rule lives in
/// [`CreditAccount`]; this only owns the account-ownership and layout checks.
fn with_credit<F>(program_id: &Pubkey, credit: &AccountInfo, f: F) -> ProgramResult
where
    F: FnOnce(&mut CreditAccount) -> Result<(), LedgerError>,
{
    require_owned(program_id, credit)?;
    let mut data = credit.try_borrow_mut_data()?;
    if data.len() < ACCOUNT_SIZE {
        return Err(custom(ERR_BAD_ACCOUNT_SIZE));
    }
    let mut account = CreditAccount::decode(&data[..])?;
    f(&mut account)?;
    data[..ACCOUNT_SIZE].copy_from_slice(&account.encode());
    Ok(())
}

/// Write a freshly created account's bytes. The runtime grows the account's
/// data region through the `create_account` CPI, so a short borrow here means
/// the creation did not take effect and writing would be a lie.
fn write_all(account: &AccountInfo, bytes: &[u8]) -> ProgramResult {
    let mut data = account.try_borrow_mut_data()?;
    if data.len() < bytes.len() {
        return Err(custom(ERR_BAD_ACCOUNT_SIZE));
    }
    data[..bytes.len()].copy_from_slice(bytes);
    Ok(())
}

/// Read the authority bound at `InitConfig`.
fn config_authority(program_id: &Pubkey, config: &AccountInfo) -> Result<Pubkey, ProgramError> {
    let (expected, _) = config_pda(program_id);
    if config.key != &expected {
        return Err(custom(ERR_BAD_PDA));
    }
    require_owned(program_id, config)?;
    let data = config.try_borrow_data()?;
    if data.len() < CONFIG_SIZE || data[0..4] != CONFIG_MAGIC {
        return Err(custom(ERR_CONFIG_NOT_INITIALISED));
    }
    let mut key = [0u8; 32];
    key.copy_from_slice(&data[4..36]);
    Ok(Pubkey::new_from_array(key))
}

/// The mint/settle gate: the signer must be the authority the config names.
fn require_authority(
    program_id: &Pubkey,
    signer: &AccountInfo,
    config: &AccountInfo,
) -> ProgramResult {
    require_signer(signer)?;
    if &config_authority(program_id, config)? != signer.key {
        return Err(custom(ERR_NOT_AUTHORITY));
    }
    Ok(())
}

/// The account must be the canonical credit PDA of `owner`, so `owner` is who
/// the bytes say it is.
fn require_credit_pda(program_id: &Pubkey, owner: &Pubkey, credit: &Pubkey) -> ProgramResult {
    if &credit_pda(program_id, owner).0 != credit {
        return Err(custom(ERR_BAD_PDA));
    }
    Ok(())
}

fn require_signer(account: &AccountInfo) -> ProgramResult {
    if !account.is_signer {
        return Err(ProgramError::MissingRequiredSignature);
    }
    Ok(())
}

/// The bytes must already belong to this program before we rewrite them.
fn require_owned(program_id: &Pubkey, account: &AccountInfo) -> ProgramResult {
    if account.owner != program_id {
        return Err(custom(ERR_ACCOUNT_NOT_OWNED));
    }
    Ok(())
}

fn require_system_program(account: &AccountInfo) -> ProgramResult {
    if !system_program::check_id(account.key) {
        return Err(custom(ERR_BAD_PDA));
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::state::ACCOUNT_MAGIC;

    fn program() -> Pubkey {
        Pubkey::new_from_array([9u8; 32])
    }

    #[test]
    fn pdas_are_deterministic_and_owner_specific() {
        let p = program();
        let a = Pubkey::new_from_array([1u8; 32]);
        let b = Pubkey::new_from_array([2u8; 32]);
        assert_eq!(credit_pda(&p, &a).0, credit_pda(&p, &a).0);
        assert_ne!(credit_pda(&p, &a).0, credit_pda(&p, &b).0);
        assert_ne!(credit_pda(&p, &a).0, config_pda(&p).0);
        // A different program id must not share the address space.
        let other = Pubkey::new_from_array([8u8; 32]);
        assert_ne!(credit_pda(&p, &a).0, credit_pda(&other, &a).0);
    }

    #[test]
    fn listing_and_escrow_pdas_are_deterministic_and_distinct() {
        let p = program();
        let a = Pubkey::new_from_array([1u8; 32]);
        let b = Pubkey::new_from_array([2u8; 32]);
        // A seller may hold several listings: the nonce is what separates them.
        assert_ne!(listing_pda(&p, &a, 1).0, listing_pda(&p, &a, 2).0);
        assert_ne!(listing_pda(&p, &a, 1).0, listing_pda(&p, &b, 1).0);
        assert_eq!(listing_pda(&p, &a, 1).0, listing_pda(&p, &a, 1).0);
        // A listing is not a credit account, and its escrow is its own PDA.
        assert_ne!(listing_pda(&p, &a, 1).0, credit_pda(&p, &a).0);
        let l = listing_pda(&p, &a, 1).0;
        assert_ne!(escrow_pda(&p, &l).0, l);
        assert_ne!(escrow_pda(&p, &l).0, credit_pda(&p, &l).0);
        assert_ne!(escrow_pda(&p, &l).0, escrow_pda(&p, &b).0);
    }

    #[test]
    fn the_token_program_bytes_are_the_canonical_spid() {
        // The buy CPI trusts these bytes to be SPL Token. Pin them to the
        // well-known id so a typo cannot point the CPI at another program.
        let expected = solana_program::pubkey::Pubkey::new_from_array(TOKEN_PROGRAM_ID);
        assert_eq!(
            expected.to_string(),
            "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"
        );
        assert_eq!(SPL_TRANSFER_CHECKED, 12);
    }

    #[test]
    fn ledger_rejections_map_to_stable_custom_codes() {
        // The wire code is part of the deployed interface: a client already
        // ships a lookup table, so a reordered enum would misreport errors.
        let cases = [
            (LedgerError::InsufficientSpendable, 1),
            (LedgerError::InsufficientLocked, 2),
            (LedgerError::RootAlreadyCommitted, 3),
            (LedgerError::UsageIdsNotMonotonic, 4),
            (LedgerError::ZeroCreditsSettle, 5),
            (LedgerError::Overflow, 6),
            (LedgerError::BadAccountData, 7),
            (LedgerError::ListingNotOpen, 8),
        ];
        for (err, code) in cases {
            assert_eq!(ProgramError::from(err), ProgramError::Custom(code));
        }
    }

    #[test]
    fn config_magic_is_not_a_credit_magic() {
        assert_ne!(CONFIG_MAGIC, ACCOUNT_MAGIC);
        assert_eq!(CONFIG_SIZE, 4 + 32 + 32 + 32);
    }

    #[test]
    fn processor_error_codes_do_not_collide() {
        // These share one namespace on the wire, so a duplicate would report
        // the wrong sentence for a real failure.
        let codes = [
            ERR_NOT_AUTHORITY,
            ERR_BAD_PDA,
            ERR_CONFIG_NOT_INITIALISED,
            ERR_ACCOUNT_NOT_OWNED,
            ERR_BAD_ACCOUNT_SIZE,
            ERR_CONFIG_ALREADY_INITIALISED,
            ERR_LISTING_NOT_OPEN,
            ERR_NOT_LISTING_SELLER,
            ERR_SELF_BUY,
            ERR_BAD_LISTING_TERMS,
            ERR_LISTING_ALREADY_OPEN,
            ERR_BAD_TOKEN_PROGRAM,
            ERR_BAD_TOKEN_ACCOUNT,
            ERR_MINT_MISMATCH,
        ];
        for (i, a) in codes.iter().enumerate() {
            assert!(*a >= 100, "processor codes start at 100: {a}");
            for b in &codes[i + 1..] {
                assert_ne!(a, b, "processor error codes must be distinct");
            }
        }
    }
}
