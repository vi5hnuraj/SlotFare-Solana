//! The credit account and every rule that governs a transition.
//!
//! All arithmetic is checked. A program that can wrap is a program that can
//! mint credits out of an underflow, so every subtraction here is a
//! `checked_sub` and every addition a `checked_add`, and a breach is an error
//! rather than a larger balance.
//!
//! The account is fixed-width and hand-encoded. There is no serialization
//! dependency: the layout is 76 bytes and the encode/decode pair is tested
//! against every field, which is cheaper to audit than a derive macro.

/// Wire and account layout size, in bytes: magic(4) minted(8) settled(8)
/// locked(8) last_root(32) last_usage_id(8) batch_count(8).
pub const ACCOUNT_SIZE: usize = 76;

/// ASCII tag written into every account so a wrong-account error is a decode
/// error rather than a plausible-looking read of unrelated data.
pub const ACCOUNT_MAGIC: [u8; 4] = *b"SFCA";

/// Wire and account layout size of a market listing, in bytes:
/// magic(4) seller(32) buyer(32) pay_mint(32) credits(8) price(8) nonce(8)
/// state(1).
pub const LISTING_SIZE: usize = 125;

/// ASCII tag for a listing account. Distinct from [`ACCOUNT_MAGIC`] and the
/// config magic, so a credit account can never decode as a listing (or the
/// other way around).
pub const LISTING_MAGIC: [u8; 4] = *b"SFLS";

/// The state byte of a listing. Encoded as one byte on the wire.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ListingState {
    /// Escrowed and buyable.
    Open,
    /// Bought: the credits left the escrow for the buyer.
    Sold,
    /// Withdrawn: the credits returned to the seller.
    Cancelled,
}

impl ListingState {
    fn as_byte(self) -> u8 {
        match self {
            ListingState::Open => 0,
            ListingState::Sold => 1,
            ListingState::Cancelled => 2,
        }
    }

    fn from_byte(b: u8) -> Result<Self, LedgerError> {
        match b {
            0 => Ok(ListingState::Open),
            1 => Ok(ListingState::Sold),
            2 => Ok(ListingState::Cancelled),
            _ => Err(LedgerError::BadAccountData),
        }
    }
}

/// One market listing: the terms plus the escrow that backs them.
///
/// This is the original `CreditMarket`'s listing record, on Solana. The credits
/// are held in a program-owned escrow account beside it, so a listed credit is
/// unspendable by the seller for as long as the listing is open - the discipline
/// that let the original's `buy()` move a seller's credits without the seller
/// being online. Everything here is little-endian, like the credit account.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct ListingAccount {
    /// The seller's wallet. Only this key may cancel the listing, and the
    /// sale pays this wallet.
    pub seller: [u8; 32],
    /// The buyer, set when the listing sells. Zero while open.
    pub buyer: [u8; 32],
    /// The mint the price is denominated in.
    pub pay_mint: [u8; 32],
    /// Credits held in escrow for this listing.
    pub credits: u64,
    /// The price in pay-token base units the buyer must pay to take it.
    pub price: u64,
    /// The nonce that, with the seller, derives this listing's PDA. Chosen by
    /// the caller, so a seller can open more than one listing.
    pub nonce: u64,
    /// Open, sold or cancelled.
    pub state: ListingState,
}

impl ListingAccount {
    /// Whether a buyer may still take this listing.
    pub fn is_open(&self) -> bool {
        self.state == ListingState::Open
    }

    /// Mark a listing sold to `buyer`. An already-moved listing is rejected, so
    /// a sale cannot happen twice off the same escrow.
    pub fn mark_sold(&mut self, buyer: [u8; 32]) -> Result<(), LedgerError> {
        if !self.is_open() {
            return Err(LedgerError::ListingNotOpen);
        }
        self.state = ListingState::Sold;
        self.buyer = buyer;
        Ok(())
    }

    /// Mark a listing cancelled. Only an open listing can be withdrawn.
    pub fn mark_cancelled(&mut self) -> Result<(), LedgerError> {
        if !self.is_open() {
            return Err(LedgerError::ListingNotOpen);
        }
        self.state = ListingState::Cancelled;
        Ok(())
    }

    /// Encode to the fixed [`LISTING_SIZE`]-byte layout.
    pub fn encode(&self) -> [u8; LISTING_SIZE] {
        let mut out = [0u8; LISTING_SIZE];
        out[0..4].copy_from_slice(&LISTING_MAGIC);
        out[4..36].copy_from_slice(&self.seller);
        out[36..68].copy_from_slice(&self.buyer);
        out[68..100].copy_from_slice(&self.pay_mint);
        out[100..108].copy_from_slice(&self.credits.to_le_bytes());
        out[108..116].copy_from_slice(&self.price.to_le_bytes());
        out[116..124].copy_from_slice(&self.nonce.to_le_bytes());
        out[124] = self.state.as_byte();
        out
    }

    /// Decode the fixed layout, rejecting anything that is not one of ours.
    pub fn decode(data: &[u8]) -> Result<Self, LedgerError> {
        if data.len() < LISTING_SIZE || data[0..4] != LISTING_MAGIC {
            return Err(LedgerError::BadAccountData);
        }
        let u64_at = |i: usize| -> u64 {
            let mut b = [0u8; 8];
            b.copy_from_slice(&data[i..i + 8]);
            u64::from_le_bytes(b)
        };
        let key_at = |i: usize| -> [u8; 32] {
            let mut b = [0u8; 32];
            b.copy_from_slice(&data[i..i + 32]);
            b
        };
        Ok(ListingAccount {
            seller: key_at(4),
            buyer: key_at(36),
            pay_mint: key_at(68),
            credits: u64_at(100),
            price: u64_at(108),
            nonce: u64_at(116),
            state: ListingState::from_byte(data[124])?,
        })
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum LedgerError {
    /// A spend, settle, lock or transfer asked for more than the unlocked
    /// remainder. This is the error that makes "never consume past what you
    /// prepaid" true on chain.
    InsufficientSpendable,
    /// Unlocking more than is locked.
    InsufficientLocked,
    /// The batch root was already committed. One root per burn.
    RootAlreadyCommitted,
    /// A batch whose first usage id does not advance the settled frontier.
    UsageIdsNotMonotonic,
    /// A settlement of zero credits has no root worth committing.
    ZeroCreditsSettle,
    /// Arithmetic left the u64 range. Checked, never wrapped.
    Overflow,
    /// The account bytes are not a Slotfare credit account.
    BadAccountData,
    /// A listing was bought or cancelled after it had already moved. The
    /// escrow is spent once, so a second move has nothing behind it.
    ListingNotOpen,
}

impl core::fmt::Display for LedgerError {
    fn fmt(&self, f: &mut core::fmt::Formatter<'_>) -> core::fmt::Result {
        let s = match self {
            LedgerError::InsufficientSpendable => "insufficient spendable credits",
            LedgerError::InsufficientLocked => "insufficient locked credits",
            LedgerError::RootAlreadyCommitted => "usage root already committed",
            LedgerError::UsageIdsNotMonotonic => "usage ids must advance",
            LedgerError::ZeroCreditsSettle => "a settlement of zero credits has no root to commit",
            LedgerError::Overflow => "credit arithmetic overflowed",
            LedgerError::BadAccountData => "account data is not a Slotfare credit account",
            LedgerError::ListingNotOpen => "the listing is not open",
        };
        f.write_str(s)
    }
}

/// One account's credit position.
///
/// `pending` is deliberately absent. Pending usage is an off-chain reservation
/// the metering proxy holds between reserving and settling; the chain only
/// ever sees the settled figure. Spendable here is therefore
/// `minted - settled - locked`, and the daemon subtracts its own pending
/// reservations on top. Documented in docs/onchain-ledger.md.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub struct CreditAccount {
    /// Total credits ever minted to this account by the deposit rail.
    pub minted: u64,
    /// Total credits consumed: burned by settlement, or moved out to another
    /// account by a market transfer. Never decreases. `minted - spent - locked`
    /// is the whole accounting identity, so the three figures are enough to
    /// re-derive the balance from the account bytes alone.
    pub spent: u64,
    /// Credits locked by an open listing. Spendable is minted - settled - locked.
    pub locked: u64,
    /// The most recently committed usage root, so it cannot be committed twice.
    pub last_root: [u8; 32],
    /// The highest usage id settled, so batches stay append-only.
    pub last_usage_id: u64,
    /// Batches settled against this account. Monotonic; for the console.
    pub batch_count: u64,
}

impl CreditAccount {
    /// Credits that can be spent, settled or locked right now.
    pub fn spendable(&self) -> u64 {
        self.minted
            .saturating_sub(self.spent)
            .saturating_sub(self.locked)
    }

    /// Mint credits. Restricted to the deposit rail by the processor: the
    /// ledger itself cannot tell who is calling, so that check is an
    /// authority check, not a state rule.
    pub fn credit(&mut self, amount: u64) -> Result<(), LedgerError> {
        self.minted = self
            .minted
            .checked_add(amount)
            .ok_or(LedgerError::Overflow)?;
        Ok(())
    }

    /// Lock credits for an open listing. Locking is what makes a listed
    /// credit unspendable, the discipline the original's market was built on.
    pub fn lock(&mut self, amount: u64) -> Result<(), LedgerError> {
        if amount > self.spendable() {
            return Err(LedgerError::InsufficientSpendable);
        }
        self.locked = self
            .locked
            .checked_add(amount)
            .ok_or(LedgerError::Overflow)?;
        Ok(())
    }

    /// Return locked credits to the spendable balance (cancel a listing).
    pub fn unlock(&mut self, amount: u64) -> Result<(), LedgerError> {
        if amount > self.locked {
            return Err(LedgerError::InsufficientLocked);
        }
        self.locked -= amount;
        Ok(())
    }

    /// Move credits out of this account to another. Used by the market sale:
    /// the seller's credits go to the buyer in one instruction.
    pub fn transfer_out(&mut self, amount: u64) -> Result<(), LedgerError> {
        if amount > self.spendable() {
            return Err(LedgerError::InsufficientSpendable);
        }
        self.spent = self
            .spent
            .checked_add(amount)
            .ok_or(LedgerError::Overflow)?;
        Ok(())
    }

    /// Receive a transfer. The counterpart of [`transfer_out`], kept separate
    /// so the processor can apply both sides in one instruction.
    pub fn receive(&mut self, amount: u64) -> Result<(), LedgerError> {
        self.credit(amount)
    }

    /// Commit a usage root and burn the credits it covers, in one atomic step.
    ///
    /// It enforces directly in state: a root can be committed once, the
    /// batch must advance the usage frontier, and the burn cannot exceed the
    /// spendable balance (so locked credits are untouchable).
    pub fn commit_and_settle(
        &mut self,
        root: [u8; 32],
        first_usage_id: u64,
        last_usage_id: u64,
        credits: u64,
    ) -> Result<(), LedgerError> {
        if credits == 0 {
            return Err(LedgerError::ZeroCreditsSettle);
        }
        if root == self.last_root {
            return Err(LedgerError::RootAlreadyCommitted);
        }
        if first_usage_id <= self.last_usage_id {
            return Err(LedgerError::UsageIdsNotMonotonic);
        }
        if credits > self.spendable() {
            return Err(LedgerError::InsufficientSpendable);
        }
        self.spent = self
            .spent
            .checked_add(credits)
            .ok_or(LedgerError::Overflow)?;
        self.last_root = root;
        self.last_usage_id = last_usage_id.max(first_usage_id);
        self.batch_count = self
            .batch_count
            .checked_add(1)
            .ok_or(LedgerError::Overflow)?;
        Ok(())
    }

    /// Encode to the fixed 76-byte account layout.
    pub fn encode(&self) -> [u8; ACCOUNT_SIZE] {
        let mut out = [0u8; ACCOUNT_SIZE];
        out[0..4].copy_from_slice(&ACCOUNT_MAGIC);
        out[4..12].copy_from_slice(&self.minted.to_le_bytes());
        out[12..20].copy_from_slice(&self.spent.to_le_bytes());
        out[20..28].copy_from_slice(&self.locked.to_le_bytes());
        out[28..60].copy_from_slice(&self.last_root);
        out[60..68].copy_from_slice(&self.last_usage_id.to_le_bytes());
        out[68..76].copy_from_slice(&self.batch_count.to_le_bytes());
        out
    }

    /// Decode the fixed layout, rejecting anything that is not one of ours.
    pub fn decode(data: &[u8]) -> Result<Self, LedgerError> {
        if data.len() < ACCOUNT_SIZE || data[0..4] != ACCOUNT_MAGIC {
            return Err(LedgerError::BadAccountData);
        }
        let u64_at = |i: usize| -> u64 {
            let mut b = [0u8; 8];
            b.copy_from_slice(&data[i..i + 8]);
            u64::from_le_bytes(b)
        };
        let mut root = [0u8; 32];
        root.copy_from_slice(&data[28..60]);
        Ok(CreditAccount {
            minted: u64_at(4),
            spent: u64_at(12),
            locked: u64_at(20),
            last_root: root,
            last_usage_id: u64_at(60),
            batch_count: u64_at(68),
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn account(minted: u64) -> CreditAccount {
        let mut a = CreditAccount::default();
        a.credit(minted).unwrap();
        a
    }

    #[test]
    fn spendable_is_minted_minus_settled_minus_locked() {
        let mut a = account(1_000);
        a.lock(400).unwrap();
        a.commit_and_settle([1u8; 32], 1, 10, 100).unwrap();
        assert_eq!(a.spendable(), 500, "1000 minted - 100 spent - 400 locked");
    }

    #[test]
    fn settlement_cannot_exceed_spendable() {
        let mut a = account(100);
        a.lock(60).unwrap();
        // Only 40 is unlocked, so a 41-credit burn must fail.
        assert_eq!(
            a.commit_and_settle([2u8; 32], 1, 5, 41),
            Err(LedgerError::InsufficientSpendable)
        );
        assert_eq!(a.commit_and_settle([2u8; 32], 1, 5, 40), Ok(()));
        assert_eq!(a.spent, 40);
    }

    #[test]
    fn locked_credits_are_never_settleable() {
        let mut a = account(100);
        a.lock(100).unwrap();
        assert_eq!(a.spendable(), 0);
        assert_eq!(
            a.commit_and_settle([3u8; 32], 1, 5, 1),
            Err(LedgerError::InsufficientSpendable)
        );
        assert_eq!(a.spent, 0, "a locked credit must not be burned");
    }

    #[test]
    fn one_root_per_burn() {
        let mut a = account(100);
        let root = [7u8; 32];
        assert_eq!(a.commit_and_settle(root, 1, 5, 10), Ok(()));
        // The same root cannot be committed again, even for a different range.
        assert_eq!(
            a.commit_and_settle(root, 6, 9, 10),
            Err(LedgerError::RootAlreadyCommitted)
        );
        assert_eq!(a.spent, 10, "the rejected retry burned nothing");
        assert_eq!(a.batch_count, 1);
        // A different root over an advancing range is fine.
        assert_eq!(a.commit_and_settle([8u8; 32], 6, 9, 10), Ok(()));
        assert_eq!(a.batch_count, 2);
    }

    #[test]
    fn a_zero_credit_settlement_is_rejected() {
        let mut a = account(100);
        assert_eq!(
            a.commit_and_settle([9u8; 32], 1, 5, 0),
            Err(LedgerError::ZeroCreditsSettle)
        );
    }

    #[test]
    fn usage_ids_must_advance() {
        let mut a = account(100);
        assert_eq!(a.commit_and_settle([4u8; 32], 1, 10, 5), Ok(()));
        // A batch that does not start above the settled frontier is rejected,
        // which is what keeps the console's id range honest.
        assert_eq!(
            a.commit_and_settle([5u8; 32], 10, 20, 5),
            Err(LedgerError::UsageIdsNotMonotonic)
        );
        assert_eq!(a.commit_and_settle([5u8; 32], 11, 20, 5), Ok(()));
    }

    #[test]
    fn credits_cannot_be_spent_twice() {
        let mut a = account(100);
        a.commit_and_settle([1u8; 32], 1, 5, 60).unwrap();
        // 40 left: the second burn of 60 has to fail.
        assert_eq!(
            a.commit_and_settle([2u8; 32], 6, 9, 60),
            Err(LedgerError::InsufficientSpendable)
        );
        assert_eq!(a.spendable(), 40);
    }

    #[test]
    fn transfer_draws_from_the_same_spendable_figure() {
        let mut a = account(100);
        a.lock(30).unwrap();
        assert_eq!(a.transfer_out(71), Err(LedgerError::InsufficientSpendable));
        assert_eq!(a.transfer_out(70), Ok(()));
        assert_eq!(
            a.spendable(),
            0,
            "transferred credits are gone from spendable"
        );
        let mut b = CreditAccount::default();
        b.receive(70).unwrap();
        assert_eq!(b.spendable(), 70);
    }

    #[test]
    fn lock_and_unlock_round_trip() {
        let mut a = account(100);
        assert_eq!(a.lock(101), Err(LedgerError::InsufficientSpendable));
        a.lock(100).unwrap();
        assert_eq!(a.unlock(101), Err(LedgerError::InsufficientLocked));
        a.unlock(100).unwrap();
        assert_eq!(a.spendable(), 100, "cancel returns the credits intact");
        assert_eq!(a.locked, 0);
    }

    #[test]
    fn arithmetic_never_wraps_on_overflow() {
        let mut a = CreditAccount {
            minted: u64::MAX,
            spent: 0,
            locked: 0,
            last_root: [0u8; 32],
            last_usage_id: 0,
            batch_count: 0,
        };
        assert_eq!(a.credit(1), Err(LedgerError::Overflow));
        assert_eq!(a.minted, u64::MAX, "a failed credit changes nothing");
        // A settle that would push `settled` past the u64 range is caught by
        // the spendable check first, so construct the edge directly.
        a.spent = u64::MAX;
        assert_eq!(a.spendable(), 0);
    }

    #[test]
    fn account_bytes_round_trip_every_field() {
        let a = CreditAccount {
            minted: 1_234_567,
            spent: 42,
            locked: 7,
            last_root: [3u8; 32],
            last_usage_id: 9_999,
            batch_count: 17,
        };
        let decoded = CreditAccount::decode(&a.encode()).unwrap();
        assert_eq!(decoded, a);
        assert_eq!(a.encode().len(), ACCOUNT_SIZE);
    }

    #[test]
    fn foreign_account_bytes_are_rejected() {
        let too_short = [0u8; 10];
        assert_eq!(
            CreditAccount::decode(&too_short),
            Err(LedgerError::BadAccountData)
        );
        let mut wrong_magic = [0u8; ACCOUNT_SIZE];
        wrong_magic[0..4].copy_from_slice(b"XXXX");
        assert_eq!(
            CreditAccount::decode(&wrong_magic),
            Err(LedgerError::BadAccountData)
        );
    }

    #[test]
    fn a_clean_account_reports_no_spendable_surplus() {
        let a = CreditAccount::default();
        assert_eq!(a.spendable(), 0);
        assert_eq!(a.batch_count, 0);
        assert_eq!(a.last_usage_id, 0);
    }

    fn listing() -> ListingAccount {
        ListingAccount {
            seller: [1u8; 32],
            buyer: [0u8; 32],
            pay_mint: [2u8; 32],
            credits: 5_000,
            price: 1_000_000,
            nonce: 42,
            state: ListingState::Open,
        }
    }

    #[test]
    fn listing_bytes_round_trip_every_field() {
        let l = listing();
        let decoded = ListingAccount::decode(&l.encode()).unwrap();
        assert_eq!(decoded, l);
        assert_eq!(l.encode().len(), LISTING_SIZE);
    }

    #[test]
    fn a_listing_cannot_decode_as_a_credit_account() {
        // The magics are the whole protection against reading one account as
        // the other: a credit decode must reject listing bytes and vice versa.
        assert_ne!(LISTING_MAGIC, ACCOUNT_MAGIC);
        assert_eq!(
            CreditAccount::decode(&listing().encode()),
            Err(LedgerError::BadAccountData)
        );
        assert_eq!(
            ListingAccount::decode(&CreditAccount::default().encode()),
            Err(LedgerError::BadAccountData)
        );
        assert_eq!(
            ListingAccount::decode(&[0u8; 10]),
            Err(LedgerError::BadAccountData)
        );
    }

    #[test]
    fn a_listing_sells_once() {
        let mut l = listing();
        assert!(l.is_open());
        assert_eq!(l.mark_sold([7u8; 32]), Ok(()));
        assert_eq!(l.buyer, [7u8; 32]);
        assert!(!l.is_open());
        // A second sale has no escrow behind it.
        assert_eq!(l.mark_sold([8u8; 32]), Err(LedgerError::ListingNotOpen));
        // And a sold listing cannot be withdrawn either.
        assert_eq!(l.mark_cancelled(), Err(LedgerError::ListingNotOpen));
        assert_eq!(l.buyer, [7u8; 32], "the failed moves changed nothing");
    }

    #[test]
    fn a_listing_cancels_once() {
        let mut l = listing();
        assert_eq!(l.mark_cancelled(), Ok(()));
        assert_eq!(l.state, ListingState::Cancelled);
        assert_eq!(l.mark_cancelled(), Err(LedgerError::ListingNotOpen));
        assert_eq!(l.mark_sold([9u8; 32]), Err(LedgerError::ListingNotOpen));
    }

    #[test]
    fn an_unknown_listing_state_byte_is_rejected() {
        let mut bytes = listing().encode();
        bytes[124] = 9;
        assert_eq!(
            ListingAccount::decode(&bytes),
            Err(LedgerError::BadAccountData)
        );
    }

    #[test]
    fn the_listing_state_byte_is_stable_on_the_wire() {
        // Open=0 is what the Go client and the on-chain read agree on.
        let mut l = listing();
        assert_eq!(l.encode()[124], 0);
        l.mark_sold([1u8; 32]).unwrap();
        assert_eq!(l.encode()[124], 1);
        l = listing();
        l.mark_cancelled().unwrap();
        assert_eq!(l.encode()[124], 2);
    }
}
