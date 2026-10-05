// Package console is the session handshake for the attest-rpc console: it
// proves which account a visitor controls and hands back a bearer token that
// identifies that account to the console's own handlers. The token is a
// persisted, sliding session credential: it survives slotfared restarts and its
// 14-day expiry refreshes on use, so an active user never signs in again.
// The same token manages the account's API keys and reads its balance.
//
// The handler is mounted under /console with http.StripPrefix("/console", h),
// so the full paths are /console/session/nonce, /console/session/verify,
// /console/session/secret, /console/keys, /console/keys/{prefix},
// /console/account, /console/usage, /console/funding, /console/chains,
// /console/market, /console/listings, /console/models, /console/attestation
// and /console/stream.
// All responses are plain JSON (not JSON-RPC): success bodies per route,
// error bodies {"error":"<reason>"}; /console/stream is a
// server-sent event stream instead.
//
// Identity is derived, not stored: accountId for a wallet session is
// 0x-hex of the raw 32-byte Solana public key, the same encoding the
// deposit watcher uses, so the wallet that signs in IS the wallet that
// deposits, and anyone can recompute the id from the public key alone.
//
// Signature scheme is pinned: the client signs the sign-in message the
// server builds from the stored nonce with its ed25519 key, and the
// 64-byte signature is accepted as hex (with or without a 0x prefix) or
// base58. This is exactly what Phantom's signMessage produces; the console
// relies on it.
package console

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"slotfare/internal/keys"
	"slotfare/internal/market"
	"slotfare/internal/meter"
	"slotfare/internal/session"
)

// Defaults for the sign-in message. Both are empty on purpose: the domain
// and URI are derived per request from the page that asked for the nonce
// (see signInDomainURI), because a wallet validates them against the
// requesting page. A non-empty value here pins them instead.
const (
	defaultDomain = ""
	defaultURI    = ""
)

// Handler owns the session nonce store and the token store. It implements
// http.Handler for the session routes plus the key and account routes;
// other handlers wrap themselves in its Middleware to resolve a bearer
// token to an account id.
type Handler struct {
	// Domain and URI are the sign-in message's origin. Empty derives them
	// from the requesting page (the normal case); non-empty pins them.
	Domain string // sign-in message domain
	URI    string // sign-in message URI

	Keys     keys.Store    // nil disables the key routes (501)
	Sessions session.Store // persisted session tokens; nil disables the token routes
	Ledger   *meter.Ledger // nil disables the account route (501)
	Usage    UsageReader   // nil disables the usage route and stream usage events (501)
	Proof    ProofReader   // nil disables the /usage/{id}/proof route (501)
	Funding  *Tracker      // nil disables the funding route (501)
	Chains   []ChainInfo   // the configured chain rows served by /chains
	Market   market.Reader // nil disables /market and /listings (501)
	// MarketWrite is the write surface (create/cancel/pay); nil disables
	// the POST routes (501) while the browse routes keep working.
	MarketWrite market.WriteStore
	// Payment verifies a market payment on the settlement chain; nil
	// (chain surface off) disables the pay route (501).
	Payment PaymentVerifier
	// Program is the deployed credit-market program: the escrow listings and
	// the three wallet-signed instructions that open, take and withdraw one.
	// nil (no SLOTFARE_LEDGER_PROGRAM) leaves the /listings/onchain routes
	// answering 501, and only the local-mirror market is served.
	Program LedgerMarket
	// Atomic persists lock and market-adjust changes to the shared
	// authority alongside the in-memory ledger; nil keeps them in memory.
	Atomic meter.AtomicLedger
	// Deposit is the Add-credits screen's target: treasury, mint, rate.
	// nil (chain surface off) reports 501.
	Deposit     *DepositTarget
	Attestation *AttestHealth // nil disables /attestation (501)
	Models      meter.Models  // the inference model catalog for /models; nil disables it (501)
	BaseURL     string        // the advertised OpenAI-compatible base URL served by /models; empty omits it
	// Minted reads an account's minted credits from the ledger contract. It
	// is consulted once for an account the in-memory ledger has never seen,
	// so a wallet that funded itself but has not spent yet sees its balance;
	// after that the settlement loop keeps the figure fresh. nil skips it.
	Minted MintedReader
	// LedgerSink, when set, also writes the first-sight seed into the
	// Postgres balance authority, so every replica's reserve sees the
	// account's minted value, not just this one's display cache. nil leaves
	// the sqlite path as-is (in-memory ledger only).
	LedgerSink func(ctx context.Context, accountID string, mintedMicro uint64) error
	// Accounts, when set, is where the account route and the balance stream
	// read the credit position from: the shared authority (Postgres) rather
	// than this replica's in-memory ledger, which only knows the
	// reservations it served and the locks it pushed. nil reads the ledger.
	Accounts meter.AccountReader

	// maturation is how long a fresh listing ages before it is buyable;
	// zero (the option's absence) means defaultListingMaturation.
	maturation time.Duration

	tokens          *tokenStore
	now             func() time.Time
	streamPoll      time.Duration
	streamHeartbeat time.Duration
}

// Option configures a Handler.
type Option func(*Handler)

// WithDomain pins the sign-in message domain instead of deriving it from
// the requesting page. Only useful where the request carries no Origin.
func WithDomain(domain string) Option {
	return func(h *Handler) { h.Domain = domain }
}

// WithURI pins the sign-in message URI instead of deriving it from the
// requesting page.
func WithURI(uri string) Option {
	return func(h *Handler) { h.URI = uri }
}

// MintedReader is the one ledger-contract read the console needs: the
// minted whole credits of an account, as the contract holds them. The
// settlement chain client satisfies it; ledger-side call sites convert to
// micro-credits.
type MintedReader interface {
	BalanceOf(ctx context.Context, accountID string) (uint64, error)
}

// WithMintedReader attaches the ledger-contract reader used to seed an
// account the in-memory ledger has never seen.
func WithMintedReader(m MintedReader) Option {
	return func(h *Handler) { h.Minted = m }
}

// WithKeys attaches the key store backing the /keys routes.
func WithKeys(s keys.Store) Option {
	return func(h *Handler) { h.Keys = s }
}

// WithSessions attaches the store backing the persisted session tokens.
// Every sign-in and every authenticated request resolves through it, so it
// must outlive the handler.
func WithSessions(s session.Store) Option {
	return func(h *Handler) { h.Sessions = s }
}

// WithLedger attaches the credit ledger backing the /account route.
func WithLedger(l *meter.Ledger) Option {
	return func(h *Handler) { h.Ledger = l }
}

// WithLedgerSink attaches the Postgres authority write for the first-sight
// seed; without it the seed lands in the in-memory ledger only.
func WithLedgerSink(sink func(ctx context.Context, accountID string, mintedMicro uint64) error) Option {
	return func(h *Handler) { h.LedgerSink = sink }
}

// WithAccountReader attaches the shared balance authority the account route
// and the balance stream read from; without it both read the in-memory
// ledger, which is only correct for a single replica.
func WithAccountReader(r meter.AccountReader) Option {
	return func(h *Handler) { h.Accounts = r }
}

// WithListingMaturation overrides how long a fresh listing ages before it
// becomes buyable (default: the CreditMarket-compatible two minutes).
func WithListingMaturation(d time.Duration) Option {
	return func(h *Handler) { h.maturation = d }
}

// WithMarketWrite attaches the market's write surface: the listing row
// load, the insert, the guarded state moves and the lock recompute.
func WithMarketWrite(m market.WriteStore) Option {
	return func(h *Handler) { h.MarketWrite = m }
}

// WithPaymentVerifier attaches the chain check the pay route runs over a
// buyer's payment signature.
func WithPaymentVerifier(p PaymentVerifier) Option {
	return func(h *Handler) { h.Payment = p }
}

// WithAtomicLedger attaches the shared authority writes for lock and
// market-adjust changes; nil keeps both in the in-memory ledger only.
func WithAtomicLedger(a meter.AtomicLedger) Option {
	return func(h *Handler) { h.Atomic = a }
}

// WithAttestation attaches the /attestation producer.
func WithAttestation(a *AttestHealth) Option {
	return func(h *Handler) { h.Attestation = a }
}

// WithDepositTarget attaches the deposit configuration the Add-credits
// screen reads.
func WithDepositTarget(d *DepositTarget) Option {
	return func(h *Handler) { h.Deposit = d }
}

// WithMarket attaches the market reader backing the /market and /listings
// routes. A *FundingStore satisfies it; without it both routes answer 501.
func WithMarket(m market.Reader) Option {
	return func(h *Handler) { h.Market = m }
}

// withNow injects the clock; tests use it to control expiry. Unexported.
func withNow(now func() time.Time) Option {
	return func(h *Handler) { h.now = now }
}

// New builds a Handler with defaults and the given options.
func New(opts ...Option) *Handler {
	h := &Handler{
		Domain:          defaultDomain,
		URI:             defaultURI,
		now:             time.Now,
		streamPoll:      defaultStreamPoll,
		streamHeartbeat: defaultStreamHeartbeat,
		maturation:      defaultListingMaturation,
	}
	for _, o := range opts {
		o(h)
	}
	h.tokens = newTokenStore(h.Sessions)
	return h
}

// ServeHTTP routes the session, key and account endpoints on the path
// relative to the mount point. Non-allowed method on a known route is a 405;
// anything else is a 404.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/session/nonce":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		h.handleNonce(w, r)
	case "/session/verify":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		h.handleVerify(w, r)
	case "/session/secret":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		h.handleSecret(w, r)
	case "/session/signout":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		h.handleSignout(w, r)
	case "/keys":
		switch r.Method {
		case http.MethodPost:
			h.handleIssueKey(w, r)
		case http.MethodGet:
			h.handleListKeys(w, r)
		default:
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	case "/account":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		h.handleAccount(w, r)
	case "/usage":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		h.handleUsage(w, r)
	case "/funding":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		h.handleFunding(w, r)
	case "/attestation":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		h.handleAttestation(w, r)
	case "/chains":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		h.handleChains(w, r)
	case "/deposit-target":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		h.handleDepositTarget(w, r)
	case "/market":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		h.handleMarket(w, r)
	case "/listings":
		switch r.Method {
		case http.MethodGet:
			h.handleListings(w, r)
		case http.MethodPost:
			h.handleCreateListing(w, r)
		default:
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	// The on-chain market's two round trips: prepare hands the wallet an
	// unsigned transaction, confirm records what the program already did.
	case "/listings/onchain/prepare":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		h.handlePrepareMarket(w, r)
	case "/listings/onchain/confirm":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		h.handleConfirmMarket(w, r)
	case "/models":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		h.handleModels(w, r)
	case "/stream":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		h.handleStream(w, r)
	default:
		if id, action, ok := parseListingWritePath(r.URL.Path); ok {
			if r.Method != http.MethodPost {
				writeError(w, http.StatusMethodNotAllowed, "method not allowed")
				return
			}
			if action == "cancel" {
				h.handleCancelListing(w, r, id)
			} else {
				h.handlePayListing(w, r, id)
			}
			return
		}
		if strings.HasPrefix(r.URL.Path, "/keys/") {
			if r.Method != http.MethodDelete {
				writeError(w, http.StatusMethodNotAllowed, "method not allowed")
				return
			}
			h.handleRevokeKey(w, r)
			return
		}
		if rowID, ok := parseUsageProofPath(r.URL.Path); ok {
			if r.Method != http.MethodGet {
				writeError(w, http.StatusMethodNotAllowed, "method not allowed")
				return
			}
			h.handleUsageProof(w, r, rowID)
			return
		}
		writeError(w, http.StatusNotFound, "not found")
	}
}

// writeJSON writes v as a JSON body with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// writeError writes the standard error body.
func writeError(w http.ResponseWriter, status int, reason string) {
	writeJSON(w, status, map[string]string{"error": reason})
}
