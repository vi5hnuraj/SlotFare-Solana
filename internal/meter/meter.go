// Package meter holds the credit accounting that sits between an API key and
// the RPC nodes. The on-chain ledger is the source of truth for credits
// minted; this package owns what has been spent since the last settlement.
// Internally every amount is in micro-credits (10,000 to the credit, see
// MicroCreditsPerCredit): the pricers convert pricing configuration from
// credits to micro-credits once, and the settler converts back to whole
// credits at the chain boundary, carrying the sub-credit remainder locally.
package meter

// Weight is the credit cost of one RPC call as configured: heavier methods
// cost more. Pricing configuration stays in credits; Weights.Cost returns
// the same figure in micro-credits, the unit the ledger reserves against.
type Weight uint64
