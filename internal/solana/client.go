// Package solana is the Slotfare-on-Solana chain surface: the credit ledger
// lives in the slotfare-ledger program and balances are minted by the USDC
// deposit watcher.
//
// Settlement then takes one of two rails. With SLOTFARE_LEDGER_PROGRAM set,
// the balance, the locks and the usage-root commitment all live in the
// slotfare-ledger program (ledger.go): a settle commits the root and burns
// the credits it covers in one on-chain transition, so the chain enforces the
// original's "no burn without a committed root, one root per burn" instead of
// merely witnessing it. Without a program, the root is posted as a Memo
// transaction and the burn happens against the local ledger - verifiable, but
// enforced only by the daemon.
package solana

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
)

// Config is the environment-derived chain configuration.
type Config struct {
	// RPC is the Solana JSON-RPC endpoint (http/https).
	RPC string
	// FeePayer is the base58-encoded private key of the tx fee payer and
	// memo signer. Never a user key.
	FeePayer string
	// Treasury is the base58 public key of the token account that receives
	// USDC deposits (the associated token account of the treasury wallet).
	Treasury string
	// Mint is the USDC SPL mint (defaults to devnet USDC).
	Mint string
	// Cluster labels explorer links: "devnet", "mainnet-beta".
	Cluster string
	// Poll is the deposit watcher's scan period; zero means the default.
	Poll time.Duration
	// LedgerProgram is the deployed slotfare-ledger program id (base58).
	// When set, balances and settlement move into that program: deposits are
	// mirrored to it and a settle burns on chain against a committed usage
	// root. When empty the daemon keeps the Memo commit and the local
	// authority it shipped with.
	LedgerProgram string
}

// LoadConfig reads the chain configuration from the environment. An empty
// RPC disables the chain surface (offline demo mode); a set-but-malformed
// value is a startup error, like a quiet misconfiguration that would sign
// memos nobody can verify.
func LoadConfig() (Config, error) {
	cfg := Config{
		RPC:           os.Getenv("SLOTFARE_SOLANA_RPC"),
		FeePayer:      os.Getenv("SLOTFARE_SOLANA_FEE_PAYER"),
		Treasury:      os.Getenv("SLOTFARE_SOLANA_TREASURY"),
		Mint:          os.Getenv("SLOTFARE_SOLANA_MINT"),
		Cluster:       envOr("SLOTFARE_SOLANA_CLUSTER", "devnet"),
		LedgerProgram: os.Getenv("SLOTFARE_LEDGER_PROGRAM"),
	}
	if cfg.RPC == "" {
		return Config{}, nil
	}
	if cfg.FeePayer == "" {
		return Config{}, errors1("SLOTFARE_SOLANA_FEE_PAYER must be set when SLOTFARE_SOLANA_RPC is set")
	}
	if cfg.Mint == "" {
		// Chain-native default: Circle's faucet USDC on devnet, native
		// USDC on mainnet. An explicit SLOTFARE_SOLANA_MINT always wins.
		if cfg.Cluster == "mainnet-beta" || cfg.Cluster == "mainnet" {
			cfg.Mint = MainnetUSDCMint
		} else {
			cfg.Mint = DevnetUSDCMint
		}
	}
	poll, err := time.ParseDuration(envOr("SLOTFARE_SOLANA_POLL", "15s"))
	if err != nil {
		return Config{}, fmt.Errorf("SLOTFARE_SOLANA_POLL: %w", err)
	}
	cfg.Poll = poll
	return cfg, nil
}

// Enabled reports whether the chain surface is configured.
func (c Config) Enabled() bool { return c.RPC != "" }

func errors1(msg string) error { return fmt.Errorf("%s", msg) }

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// BalanceFunc returns the local, deposit-derived minted balance for an
// account in whole credits. The settler injects the ledger or store read;
// solana.Client never invents a balance of its own.
type BalanceFunc func(ctx context.Context, accountID string) (uint64, error)

// Client implements meter.ChainClient and meter.UsageCommitter against a
// configured Solana cluster.
type Client struct {
	cfg     Config
	balance BalanceFunc
	payer   solana.PrivateKey
	rpc     *rpc.Client
	// ledger is the configured on-chain ledger program, nil when the daemon
	// runs without one. Every ledger method gates on it.
	ledger *solana.PublicKey
}

// NewClient parses the fee payer and builds the RPC client; it performs no
// network I/O, so a transient endpoint failure never kills startup. balance
// may be nil, which makes BalanceOf report an error instead of a wrong zero.
func NewClient(cfg Config, balance BalanceFunc) (*Client, error) {
	payer, err := solana.PrivateKeyFromBase58(cfg.FeePayer)
	if err != nil {
		return nil, fmt.Errorf("parse fee payer: %w", err)
	}
	client := &Client{
		cfg:     cfg,
		balance: balance,
		payer:   payer,
		rpc:     rpc.New(cfg.RPC),
	}
	if cfg.LedgerProgram != "" {
		// A malformed program id is a startup error, like a malformed fee
		// payer: the alternative is signing settle transactions that no
		// cluster will ever accept.
		program, err := solana.PublicKeyFromBase58(cfg.LedgerProgram)
		if err != nil {
			return nil, fmt.Errorf("parse ledger program: %w", err)
		}
		client.ledger = &program
	}
	return client, nil
}

// Config returns the loaded chain configuration.
func (c *Client) Config() Config { return c.cfg }

// BalanceOf reads the deposit-derived balance from the injected local
// source. The chain does not hold credit balances in this design; the
// ledger does, and deposits are what mint.
func (c *Client) BalanceOf(ctx context.Context, accountID string) (uint64, error) {
	if c.balance == nil {
		return 0, fmt.Errorf("solana: no balance source for %s", accountID)
	}
	return c.balance(ctx, accountID)
}
