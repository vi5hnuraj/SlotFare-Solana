//! The instruction wire format.
//!
//! Hand-encoded, no serialization dependency, fixed width. Every field is
//! little-endian, every variant is a single tag byte, and the decoder rejects
//! trailing bytes so a malformed or truncated message cannot be silently
//! reinterpreted as a different instruction. This mirrors the discipline the
//! Go side already uses for the `slotfare-usage-root/v1` memo payload: a fixed
//! layout with an explicit length check.

use crate::state::LedgerError;

/// The tag byte for each variant. Stable on the wire: never renumber.
pub const TAG_INIT_CREDIT: u8 = 0;
pub const TAG_CREDIT: u8 = 1;
pub const TAG_LOCK: u8 = 2;
pub const TAG_UNLOCK: u8 = 3;
pub const TAG_COMMIT_AND_SETTLE: u8 = 4;
pub const TAG_TRANSFER: u8 = 5;
pub const TAG_INIT_CONFIG: u8 = 6;
pub const TAG_LIST: u8 = 7;
pub const TAG_BUY: u8 = 8;
pub const TAG_CANCEL_LIST: u8 = 9;

/// Payload length of [`Instruction::InitConfig`]'s three pubkeys.
pub const INIT_CONFIG_PUBKEYS: usize = 32 * 3;

/// Payload length of the [`Instruction::List`] terms: credits(8) price(8)
/// nonce(8) pay_mint(32).
pub const LIST_TERMS: usize = 8 + 8 + 8 + 32;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Instruction {
    /// Create an empty credit account for the owner in the account list.
    InitCredit,
    /// Mint credits. Deposit-rail authority only.
    Credit { amount: u64 },
    /// Lock credits for an open listing.
    Lock { amount: u64 },
    /// Return locked credits to spendable (cancel a listing).
    Unlock { amount: u64 },
    /// Commit a usage root and burn the credits it covers, in one step.
    CommitAndSettle {
        root: [u8; 32],
        first_usage_id: u64,
        last_usage_id: u64,
        credits: u64,
    },
    /// Move spendable credits from the first account to the second.
    Transfer { amount: u64 },
    /// Create the program's single config account and bind the authority that
    /// may mint and settle. Submitted once, right after deployment.
    InitConfig {
        authority: [u8; 32],
        usdc_mint: [u8; 32],
        treasury: [u8; 32],
    },
    /// Open a market listing: move `credits` into the listing's escrow PDA at
    /// `price` base units of `pay_mint`. The seller signs. The `nonce` with the
    /// seller derives the listing's address, so one seller may hold several.
    List {
        credits: u64,
        price: u64,
        nonce: u64,
        pay_mint: [u8; 32],
    },
    /// Buy an open listing: pay the seller `price` of the listing's mint and
    /// take the escrowed credits, in one atomic transition. The buyer signs.
    /// The amounts come from the listing, so they cannot be spoofed by the
    /// caller.
    Buy,
    /// Withdraw an open listing, returning its escrow to the seller. The
    /// listing's seller signs.
    CancelList,
}

impl Instruction {
    /// Encode to the wire.
    pub fn encode(&self) -> Vec<u8> {
        let mut out = Vec::new();
        match self {
            Instruction::InitCredit => out.push(TAG_INIT_CREDIT),
            Instruction::Credit { amount } => {
                out.push(TAG_CREDIT);
                out.extend_from_slice(&amount.to_le_bytes());
            }
            Instruction::Lock { amount } => {
                out.push(TAG_LOCK);
                out.extend_from_slice(&amount.to_le_bytes());
            }
            Instruction::Unlock { amount } => {
                out.push(TAG_UNLOCK);
                out.extend_from_slice(&amount.to_le_bytes());
            }
            Instruction::CommitAndSettle {
                root,
                first_usage_id,
                last_usage_id,
                credits,
            } => {
                out.push(TAG_COMMIT_AND_SETTLE);
                out.extend_from_slice(root);
                out.extend_from_slice(&first_usage_id.to_le_bytes());
                out.extend_from_slice(&last_usage_id.to_le_bytes());
                out.extend_from_slice(&credits.to_le_bytes());
            }
            Instruction::Transfer { amount } => {
                out.push(TAG_TRANSFER);
                out.extend_from_slice(&amount.to_le_bytes());
            }
            Instruction::InitConfig {
                authority,
                usdc_mint,
                treasury,
            } => {
                out.push(TAG_INIT_CONFIG);
                out.extend_from_slice(authority);
                out.extend_from_slice(usdc_mint);
                out.extend_from_slice(treasury);
            }
            Instruction::List {
                credits,
                price,
                nonce,
                pay_mint,
            } => {
                out.push(TAG_LIST);
                out.extend_from_slice(&credits.to_le_bytes());
                out.extend_from_slice(&price.to_le_bytes());
                out.extend_from_slice(&nonce.to_le_bytes());
                out.extend_from_slice(pay_mint);
            }
            Instruction::Buy => out.push(TAG_BUY),
            Instruction::CancelList => out.push(TAG_CANCEL_LIST),
        }
        out
    }

    /// Decode the wire, rejecting anything malformed.
    pub fn decode(data: &[u8]) -> Result<Self, LedgerError> {
        let tag = *data.first().ok_or(LedgerError::BadAccountData)?;
        // `need` is the total length the tag requires, including the tag byte.
        let need = match tag {
            TAG_INIT_CREDIT => 1,
            TAG_CREDIT | TAG_LOCK | TAG_UNLOCK | TAG_TRANSFER => 9,
            TAG_COMMIT_AND_SETTLE => 1 + 32 + 8 + 8 + 8,
            TAG_INIT_CONFIG => 1 + INIT_CONFIG_PUBKEYS,
            TAG_LIST => 1 + LIST_TERMS,
            TAG_BUY | TAG_CANCEL_LIST => 1,
            _ => return Err(LedgerError::BadAccountData),
        };
        if data.len() != need {
            return Err(LedgerError::BadAccountData);
        }
        let u64_at = |i: usize| -> u64 {
            let mut b = [0u8; 8];
            b.copy_from_slice(&data[i..i + 8]);
            u64::from_le_bytes(b)
        };
        // A 32-byte field at `i`, for the pubkey-bearing variants.
        let key_at = |i: usize| -> [u8; 32] {
            let mut b = [0u8; 32];
            b.copy_from_slice(&data[i..i + 32]);
            b
        };
        Ok(match tag {
            TAG_INIT_CREDIT => Instruction::InitCredit,
            TAG_INIT_CONFIG => Instruction::InitConfig {
                authority: key_at(1),
                usdc_mint: key_at(33),
                treasury: key_at(65),
            },
            TAG_CREDIT => Instruction::Credit { amount: u64_at(1) },
            TAG_LOCK => Instruction::Lock { amount: u64_at(1) },
            TAG_UNLOCK => Instruction::Unlock { amount: u64_at(1) },
            TAG_TRANSFER => Instruction::Transfer { amount: u64_at(1) },
            TAG_BUY => Instruction::Buy,
            TAG_CANCEL_LIST => Instruction::CancelList,
            TAG_LIST => Instruction::List {
                credits: u64_at(1),
                price: u64_at(9),
                nonce: u64_at(17),
                pay_mint: key_at(25),
            },
            _ => {
                let mut root = [0u8; 32];
                root.copy_from_slice(&data[1..33]);
                Instruction::CommitAndSettle {
                    root,
                    first_usage_id: u64_at(33),
                    last_usage_id: u64_at(41),
                    credits: u64_at(49),
                }
            }
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn round_trip(i: Instruction) {
        let bytes = i.encode();
        assert_eq!(Instruction::decode(&bytes), Ok(i), "wire round trip");
    }

    #[test]
    fn every_variant_round_trips() {
        round_trip(Instruction::InitCredit);
        round_trip(Instruction::Credit { amount: 2_500_000 });
        round_trip(Instruction::Lock { amount: 10 });
        round_trip(Instruction::Unlock { amount: 10 });
        round_trip(Instruction::Transfer { amount: u64::MAX });
        round_trip(Instruction::CommitAndSettle {
            root: [0xABu8; 32],
            first_usage_id: 1,
            last_usage_id: 4_096,
            credits: 987_654,
        });
        round_trip(Instruction::InitConfig {
            authority: [1u8; 32],
            usdc_mint: [2u8; 32],
            treasury: [3u8; 32],
        });
        round_trip(Instruction::List {
            credits: 5_000,
            price: 1_000_000,
            nonce: 42,
            pay_mint: [0xCDu8; 32],
        });
        round_trip(Instruction::Buy);
        round_trip(Instruction::CancelList);
    }

    #[test]
    fn list_terms_land_in_the_right_offsets() {
        // credits/price/nonce are three adjacent u64s and the mint follows, so
        // a swapped offset would round-trip happily and price the wrong mint.
        let bytes = Instruction::List {
            credits: 0x0102030405060708,
            price: 0x1112131415161718,
            nonce: 0x2122232425262728,
            pay_mint: [0xEEu8; 32],
        }
        .encode();
        assert_eq!(bytes.len(), 1 + LIST_TERMS);
        assert_eq!(bytes[1], 0x08, "credits is little-endian");
        assert_eq!(bytes[8], 0x01);
        assert_eq!(bytes[9], 0x18, "price starts at byte 9");
        assert_eq!(bytes[17], 0x28, "nonce starts at byte 17");
        assert_eq!(bytes[25], 0xEE, "the mint starts at byte 25");
    }

    #[test]
    fn init_config_fields_land_in_the_right_offsets() {
        // The three pubkeys are positional and identical in shape, so a
        // swapped offset would round-trip happily and bind the wrong mint.
        let bytes = Instruction::InitConfig {
            authority: [0xAAu8; 32],
            usdc_mint: [0xBBu8; 32],
            treasury: [0xCCu8; 32],
        }
        .encode();
        assert_eq!(bytes.len(), 1 + INIT_CONFIG_PUBKEYS);
        assert_eq!(bytes[1], 0xAA);
        assert_eq!(bytes[33], 0xBB);
        assert_eq!(bytes[65], 0xCC);
    }

    #[test]
    fn an_unknown_tag_is_rejected() {
        assert_eq!(
            Instruction::decode(&[200]),
            Err(LedgerError::BadAccountData)
        );
    }

    #[test]
    fn a_truncated_message_is_rejected() {
        let full = Instruction::CommitAndSettle {
            root: [1u8; 32],
            first_usage_id: 1,
            last_usage_id: 2,
            credits: 3,
        }
        .encode();
        for cut in [1usize, 8, 32, full.len() - 1] {
            assert_eq!(
                Instruction::decode(&full[..cut]),
                Err(LedgerError::BadAccountData),
                "truncation at {cut} must be rejected"
            );
        }
    }

    #[test]
    fn trailing_bytes_are_rejected() {
        // An over-long body must not be reinterpreted as the same instruction.
        let mut bytes = Instruction::Credit { amount: 5 }.encode();
        bytes.push(0);
        assert_eq!(
            Instruction::decode(&bytes),
            Err(LedgerError::BadAccountData)
        );
    }

    #[test]
    fn an_empty_message_is_rejected() {
        assert_eq!(Instruction::decode(&[]), Err(LedgerError::BadAccountData));
    }

    #[test]
    fn tags_are_stable_on_the_wire() {
        // The tag byte is part of a deployed contract: changing one would
        // silently reinterpret existing clients' calls.
        assert_eq!(Instruction::InitCredit.encode(), vec![TAG_INIT_CREDIT]);
        assert_eq!(Instruction::Credit { amount: 1 }.encode()[0], TAG_CREDIT);
        assert_eq!(Instruction::Lock { amount: 1 }.encode()[0], TAG_LOCK);
        assert_eq!(Instruction::Unlock { amount: 1 }.encode()[0], TAG_UNLOCK);
        assert_eq!(
            Instruction::CommitAndSettle {
                root: [0u8; 32],
                first_usage_id: 0,
                last_usage_id: 0,
                credits: 1
            }
            .encode()[0],
            TAG_COMMIT_AND_SETTLE
        );
        assert_eq!(
            Instruction::Transfer { amount: 1 }.encode()[0],
            TAG_TRANSFER
        );
        assert_eq!(
            Instruction::List {
                credits: 1,
                price: 1,
                nonce: 1,
                pay_mint: [0u8; 32]
            }
            .encode()[0],
            TAG_LIST
        );
        assert_eq!(Instruction::Buy.encode(), vec![TAG_BUY]);
        assert_eq!(Instruction::CancelList.encode(), vec![TAG_CANCEL_LIST]);
    }
}
