//! Slotfare's on-chain credit ledger.
//!
//! This crate provides the on-chain balance and settlement authority for Slotfare.
//! Balances, locks, and marketplace listings live in program-owned PDAs, and a
//! burn is only possible against a usage root that is committed in the same
//! atomic instruction.
//!
//! Layout:
//!
//! * [`state`] - the account state and every transition rule. No dependencies.
//! * [`instruction`] - the wire format. No dependencies.
//! * `processor` - the entrypoint, behind the `onchain` feature.
//!
//! The invariants this crate exists to enforce, all of them taken from the
//! original's contracts:
//!
//! 1. spendable = minted - settled - locked. Locked credits can never be
//!    settled, sold or transferred.
//! 2. There is no burn without a committed root, and one root per burn. A
//!    batch root that has already been committed is rejected.
//! 3. Credits can never be spent twice: settlement and transfer both draw from
//!    the same spendable figure, and the arithmetic is checked, never wrapping.
//! 4. Usage ids are append-only: a batch must start above the last settled id.

pub mod instruction;
pub mod state;

#[cfg(feature = "onchain")]
pub mod processor;

pub use instruction::Instruction;
pub use state::{CreditAccount, LedgerError};
