// Command slotfared is the Slotfare-on-Solana daemon: it sits in front of the
// metered upstreams, authenticates an API key, prices requests by method
// weight and inference by tokens, reserves credits before forwarding, and
// checkpoints usage so a restart does not lose unsettled spend. Credits are
// minted by USDC deposits on Solana (watched by internal/solana) and the
// settlement loop commits each batch's usage Merkle root to Solana as a
// Memo program transaction instead of calling a ledger contract.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"slotfare/internal/console"
	"slotfare/internal/cors"
	"slotfare/internal/funding"
	"slotfare/internal/market"
	"slotfare/internal/meter"
	"slotfare/internal/proxy"
	"slotfare/internal/receipt"
	"slotfare/internal/solana"

	gsolana "github.com/gagliardetto/solana-go"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "slotfared:", err)
		os.Exit(1)
	}
}

func run() error {
	// Subcommand dispatch before flag parsing: import-sqlite copies an
	// existing SQLite database into the configured Postgres and exits.
	if len(os.Args) > 1 && os.Args[1] == "import-sqlite" {
		dbURL := os.Getenv("SLOTFARE_DATABASE_URL")
		fs := flag.NewFlagSet("slotfared import-sqlite", flag.ContinueOnError)
		fs.StringVar(&dbURL, "database-url", dbURL, "Postgres URL (postgres:// or postgresql://) to import into")
		if err := fs.Parse(os.Args[2:]); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return errors.New("usage: slotfared import-sqlite <sqlite path>")
		}
		return runImportSQLite(dbURL, fs.Arg(0))
	}
	// ledger-init initialises the on-chain ledger program and exits. It is
	// part of deployment, not of serving, so it must not fall through to
	// starting the daemon.
	if len(os.Args) > 1 && os.Args[1] == "ledger-init" {
		return runLedgerInit(os.Args[2:])
	}
	var (
		listen            = envOr("SLOTFARE_LISTEN", "127.0.0.1:8443")
		upstream          = envOr("SLOTFARE_UPSTREAM", "")
		inferenceUpstream = envOr("SLOTFARE_INFERENCE_UPSTREAM", "")
		inferencePrefix   = envOr("SLOTFARE_INFERENCE_PREFIX", "/infer")
		dbPath            = envOr("SLOTFARE_DB", "slotfared.db")
		databaseURL       = os.Getenv("SLOTFARE_DATABASE_URL")
		weightsPath       = envOr("SLOTFARE_WEIGHTS", "")
		tokenPricesPath   = envOr("SLOTFARE_TOKEN_PRICES", "")
		flushInterval     = time.Duration(0)
		flushThreshold    uint64
		settleInterval    = time.Duration(0)
		settleThreshold   uint64
	)
	var err error
	if settleInterval, err = time.ParseDuration(envOr("SLOTFARE_SETTLE_INTERVAL", "30s")); err != nil {
		return fmt.Errorf("SLOTFARE_SETTLE_INTERVAL: %w", err)
	}
	if settleThreshold, err = strconv.ParseUint(envOr("SLOTFARE_SETTLE_THRESHOLD", "100"), 10, 64); err != nil {
		return fmt.Errorf("SLOTFARE_SETTLE_THRESHOLD: %w", err)
	}
	fs := flag.NewFlagSet("slotfared", flag.ContinueOnError)
	fs.StringVar(&listen, "listen", listen, "address to listen on")
	fs.StringVar(&upstream, "upstream", upstream, "upstream RPC URL (http or https)")
	fs.StringVar(&inferenceUpstream, "inference-upstream", inferenceUpstream, "upstream URL for the inference service; empty disables the service")
	fs.StringVar(&inferencePrefix, "inference-prefix", inferencePrefix, "path prefix that routes to the inference service")
	fs.StringVar(&dbPath, "db", dbPath, "SQLite path shared by the key store and the meter checkpoint; ignored when SLOTFARE_DATABASE_URL is set")
	fs.StringVar(&databaseURL, "database-url", databaseURL, "Postgres URL (postgres:// or postgresql://); when set the Postgres backend is used and SLOTFARE_DB is ignored")
	fs.StringVar(&weightsPath, "weights", weightsPath, "weights JSON file; a missing file uses the defaults")
	fs.StringVar(&tokenPricesPath, "token-prices", tokenPricesPath, "token prices JSON file; a missing file uses the defaults")
	fs.DurationVar(&flushInterval, "flush-interval", 5*time.Second, "checkpoint flush interval")
	fs.Uint64Var(&flushThreshold, "flush-threshold", 0, "total pending credits that trigger an immediate flush; 0 flushes every tick")
	fs.DurationVar(&settleInterval, "settle-interval", settleInterval, "settlement pass interval")
	fs.Uint64Var(&settleThreshold, "settle-threshold", settleThreshold, "per-account pending credits that trigger a settle; 0 settles every account with any pending")
	if err := fs.Parse(os.Args[1:]); err != nil {
		return err
	}
	// SLOTFARE_SETTLE_THRESHOLD and --settle-threshold are configured in
	// credits, as is --flush-threshold; the settler and the flusher work in
	// micro-credits, so convert once here at the config boundary.
	settleThreshold *= meter.MicroCreditsPerCredit
	flushThreshold *= meter.MicroCreditsPerCredit

	upstreamURL, err := url.Parse(upstream)
	if err != nil {
		return fmt.Errorf("parse upstream URL %q: %w", upstream, err)
	}
	if upstreamURL.Scheme != "http" && upstreamURL.Scheme != "https" {
		return fmt.Errorf("upstream URL %q must use http or https", upstream)
	}

	weights, err := meter.LoadWeights(weightsPath)
	if err != nil {
		return err
	}

	tokenPrices, err := meter.LoadTokenPrices(tokenPricesPath)
	if err != nil {
		return err
	}

	// The inference model registry: SLOTFARE_MODELS is a JSON list of
	// {id, upstreamId, name} plus exactly one pricing form - either explicit
	// pricing (credits per million) or provider cost in USD plus a margin,
	// from which the credit prices are derived at load. Empty means today's
	// single-model behavior; malformed is a startup failure naming the
	// offending field.
	models, err := meter.ParseModels(os.Getenv("SLOTFARE_MODELS"))
	if err != nil {
		return err
	}

	keyRateLimit, err := strconv.Atoi(envOr("SLOTFARE_KEY_RATE_LIMIT", "30"))
	if err != nil || keyRateLimit < 0 {
		return fmt.Errorf("SLOTFARE_KEY_RATE_LIMIT: %q must be a non-negative integer", os.Getenv("SLOTFARE_KEY_RATE_LIMIT"))
	}
	modelRateLimit, err := strconv.Atoi(envOr("SLOTFARE_MODEL_RATE_LIMIT", "20"))
	if err != nil || modelRateLimit < 0 {
		return fmt.Errorf("SLOTFARE_MODEL_RATE_LIMIT: %q must be a non-negative integer", os.Getenv("SLOTFARE_MODEL_RATE_LIMIT"))
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Deposits are USDC transfers on Solana, watched by internal/solana:
	// there is no source-chain feeder and no chain-1 stamp to fall back on.
	// sourceChainKey remains the legacy stamp for pre-existing funding rows.
	sourceChainKey := uint64(1)

	stores, err := openStores(ctx, databaseURL, dbPath, sourceChainKey)
	if err != nil {
		return err
	}
	defer stores.Close()
	log.Printf("slotfared: storage: %s", stores.label)

	// The funding tracker owns deposit history for the console. It is built
	// before the deposit watcher so each new deposit can be recorded as it
	// is minted.
	tracker := console.NewTracker(stores.funding)

	// Opportunistic sweep at boot: sessions that outlived their 14-day
	// sliding window stop matching anyway, but the rows are gone too.
	swept, err := stores.session.SweepExpired(ctx, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("sweep expired sessions: %w", err)
	}
	log.Printf("slotfared: swept %d expired console session(s)", swept)

	ledger := meter.NewLedger()
	accounts, err := stores.meter.Load(ctx)
	if err != nil {
		return fmt.Errorf("load checkpoint: %w", err)
	}
	ledger.Restore(accounts)

	// Postgres mode: the shared accounts table is the balance authority and
	// the database coordinates the loops; sqlite mode: the in-memory ledger
	// is the request path exactly as today, checkpointed by the Flusher.
	isPG := stores.label == "postgres"
	var authority meter.Authority
	if isPG {
		authority = stores.atomic
	} else {
		authority = meter.NewLocalAuthority(ledger)
	}

	if isPG {
		// The replica-local ledger is a console display cache only; its
		// DELETE+INSERT checkpoint would clobber the stores.atomic state, so the
		// flusher does not run.
		log.Println("slotfared: flusher disabled: Postgres is the balance authority")
	} else {
		flusher := meter.NewFlusher(ledger, stores.meter, flushInterval, flushThreshold)
		go func() {
			if err := flusher.Run(ctx, func(err error) { log.Printf("slotfared: flush: %v", err) }); err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("slotfared: flusher stopped: %v", err)
			}
		}()
	}

	// The Solana chain surface: RPC + fee payer enable root commits and the
	// deposit watcher; unset runs the offline demo path with no chain at all.
	solCfg, err := solana.LoadConfig()
	if err != nil {
		return err
	}
	settleClient, err := startSettler(ctx, solCfg, ledger, stores.meter, stores.atomic, stores.locker, settleInterval, settleThreshold)
	if err != nil {
		return err
	}
	// The deposit watcher is the only minter: it rescans the treasury's
	// USDC transfer history and writes absolute minted totals to the
	// balance authority, so a restart converges instead of double-minting.
	var depositWatcher *solana.Watcher
	if solCfg.Enabled() && solCfg.Treasury != "" {
		// The deposit rail owns the write path: it refuses to lower a
		// recorded total (both storage layers enforce that themselves) and
		// returns a failed chain mirror instead of swallowing it, so the
		// watcher retries rather than marking the account done. The rules and
		// the reasons are in deposit.go.
		rail := newDepositRail(ledger, stores.atomic, newLedgerMirror(settleClient), log.Printf)
		watcher, err := solana.NewWatcher(solCfg, rail.mint)
		if err != nil {
			return fmt.Errorf("deposit watcher: %w", err)
		}
		depositWatcher = watcher
		// Every treasury deposit becomes a funding row for the console's
		// Transactions list and trace: the signature is the deposit id, so
		// the store's insert-or-ignore primary key keeps the repeated
		// rescans idempotent. Credited is immediate - the watcher mints in
		// the same pass, there is no second chain to wait for.
		watcher.OnDeposit = func(ctx context.Context, d solana.Deposit, slot uint64, blockTime time.Time) {
			p := funding.Purchase{
				AccountID:      d.AccountID,
				ChainKey:       0,
				SourceTx:       d.Signature,
				SourceBlock:    slot,
				AttestedHeight: slot,
				Credited:       true,
				DepositID:      d.Signature,
				Token:          "USDC",
				UsdMicros:      d.USDCBase,
				StartedAt:      blockTime.Format(time.RFC3339),
				CCTx:           d.Signature,
				CCBlock:        slot,
				CreditedAt:     time.Now().UTC().Format(time.RFC3339),
				Credits:        d.USDCBase * solana.MicroCreditsPerUSDCBase / meter.MicroCreditsPerCredit,
			}
			if err := tracker.Record(ctx, p); err != nil {
				log.Printf("slotfared: record deposit %s: %v", d.Signature, err)
				return
			}
			tracker.SetTip(0, slot)
			tracker.SetCeiling(0, slot)
		}
		go func() {
			if err := watcher.Run(ctx, func(err error) { log.Printf("slotfared: deposit watcher: %v", err) }); err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("slotfared: deposit watcher stopped: %v", err)
			}
		}()
		log.Printf("slotfared: deposit watcher on treasury %s (mint %s, %s)", solCfg.Treasury, solCfg.Mint, solCfg.Cluster)
	}

	// The market indexer is the event feeder's port: it replays the ledger
	// program's transaction history into the funding store's listings table
	// (idempotent on transaction signature), so the console sees marketplace
	// activity that happened outside a frontend session - a list placed from
	// a wallet, a purchase, a cancel - the same frontend session. Deposit
	// accounting stays with the deposit watcher: a purchase of credits is
	// never a deposit.
	if solCfg.LedgerProgram != "" {
		program, err := gsolana.PublicKeyFromBase58(solCfg.LedgerProgram)
		if err != nil {
			return fmt.Errorf("parse ledger program for indexer: %w", err)
		}
		mirror := market.Mirror{Store: stores.market, Program: program}
		ix, err := solana.NewIndexer(settleClient, mirror)
		if err != nil {
			return fmt.Errorf("market indexer: %w", err)
		}
		go func() {
			interval := 30 * time.Second
			if raw := envOr("SLOTFARE_INDEXER_INTERVAL_SECONDS", ""); raw != "" {
				if v, err := strconv.Atoi(raw); err == nil && v > 0 {
					interval = time.Duration(v) * time.Second
				}
			}
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
				n, err := ix.RunOnce(ctx)
				if err != nil && !errors.Is(err, context.Canceled) {
					log.Printf("slotfared: market indexer: %v", err)
				}
				if n > 0 {
					log.Printf("slotfared: market indexer applied %d event(s)", n)
				}
			}
		}()
		log.Printf("slotfared: market indexer on ledger program %s (%s)", solCfg.LedgerProgram, solCfg.Cluster)
	}

	services := []proxy.Service{
		{Name: "rpc", Prefix: "/rpc", Upstream: upstreamURL, Pricer: proxy.NewRPCPricer(weights)},
	}
	// advertisedBase is the OpenAI-compatible base URL the console's
	// /models payload advertises; it is set only when the inference
	// service is enabled, so a disabled service leaves the payload shape
	// unchanged.
	var advertisedBase string
	if inferenceUpstream != "" {
		infURL, err := url.Parse(inferenceUpstream)
		if err != nil {
			return fmt.Errorf("parse inference upstream URL %q: %w", inferenceUpstream, err)
		}
		if infURL.Scheme != "http" && infURL.Scheme != "https" {
			return fmt.Errorf("inference upstream URL %q must use http or https", inferenceUpstream)
		}
		services = append(services, proxy.Service{
			Name:     "inference",
			Prefix:   inferencePrefix,
			Upstream: infURL,
			Pricer:   proxy.NewInferencePricerWithModels(tokenPrices, models),
			// The upstream bearer replaces the client's Slotfare key on the
			// outbound request; unset keeps today's header-less forwarding.
			UpstreamKey: os.Getenv("SLOTFARE_INFERENCE_UPSTREAM_KEY"),
			// Streaming usage reporting: opt in only for upstreams that
			// support stream_options (Fireworks); the NVIDIA free tier does
			// not, so it stays off and its requests are byte-identical.
			StreamUsage: envOr("SLOTFARE_INFERENCE_STREAM_USAGE", "") == "1",
			Allow: []string{
				"/v1/models",
				"/v1/chat/completions",
				"/v1/completions",
				"/v1/embeddings",
				"/v1/rerank",
				"/health",
			},
		})
		// The advertised base must be one a key holder can actually curl:
		// SLOTFARE_PUBLIC_URL sets the public root, otherwise it is derived
		// from the listen address. The /v1 suffix is the same allowlist
		// suffix the proxy routes, so the advertised path always serves.
		advertisedBase, err = advertisedBaseURL(os.Getenv("SLOTFARE_PUBLIC_URL"), listen, inferencePrefix)
		if err != nil {
			return err
		}
	}

	handler := proxy.New(stores.keys, authority, services)
	handler.Usage = stores.meter
	// The receipt signer: SLOTFARE_RECEIPT_KEY is a dedicated Ed25519
	// private key (hex; 64-byte Solana-keypair secret or 32-byte seed, 0x
	// optional) - never a chain spender or fee-payer key. When set, metered
	// non-streaming responses carry x-slotfare-receipt and the well-known
	// path serves the signer's Solana public key; when unset receipts are
	// disabled and that path answers 404. A malformed key fails startup,
	// like any key: a quiet misconfiguration would sign receipts
	// nobody can verify.
	if receiptKey := os.Getenv("SLOTFARE_RECEIPT_KEY"); receiptKey != "" {
		signer, err := receipt.LoadHex(receiptKey)
		if err != nil {
			return fmt.Errorf("SLOTFARE_RECEIPT_KEY: %w", err)
		}
		handler.Receipt = signer
		log.Printf("slotfared: usage receipts enabled, signing address %s", signer.Address())
	} else {
		log.Println("slotfared: usage receipts disabled: SLOTFARE_RECEIPT_KEY not set")
	}
	// Both rate-limit layers are always on in production: per key and per
	// registry model, requests per minute. Zero disables a layer, which no
	// operator wants here - it would let one key or one client class
	// bombard the free upstream.
	handler.KeyRateLimit = keyRateLimit
	handler.ModelRateLimit = modelRateLimit

	// Solana is the one chain every purchase settles against: chain key 0
	// (no separate attestation chain), labelled for the console's trace.
	tracker.SetChainNames(map[uint64]string{0: "Solana"})
	tracker.SetChainExplorers(map[uint64]string{0: "https://explorer.solana.com"})

	// The console session handshake is mounted under /console; everything
	// else goes to the metering proxy. Sign-in is ed25519 over a
	// server-built message - no chain call is involved.
	chOpts := []console.Option{
		console.WithKeys(stores.keys),
		console.WithSessions(stores.session),
		console.WithLedger(ledger),
		console.WithUsage(stores.meter),
		console.WithProof(stores.meter),
		console.WithFunding(tracker),
		console.WithMarket(stores.market),
		console.WithModels(models),
		console.WithMarketWrite(stores.market),
		console.WithAtomicLedger(stores.atomic),
	}
	if dt := console.DepositTargetFor(solCfg); dt != nil {
		chOpts = append(chOpts, console.WithDepositTarget(dt))
	}
	if raw := os.Getenv("SLOTFARE_LISTING_MATURATION"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d < 0 {
			return fmt.Errorf("SLOTFARE_LISTING_MATURATION: %q must be a duration like 90s", raw)
		}
		chOpts = append(chOpts, console.WithListingMaturation(d))
	}
	if settleClient != nil {
		chOpts = append(chOpts, console.WithPaymentVerifier(settleClient))
		// The on-chain market is served only when a ledger program is
		// configured: without one the prepare route has no escrow to build a
		// transaction against, and the console stays on the local mirror.
		if settleClient.LedgerEnabled() {
			chOpts = append(chOpts, console.WithLedgerMarket(settleClient))
		}
	}
	if advertisedBase != "" {
		chOpts = append(chOpts, console.WithBaseURL(advertisedBase))
	}
	if isPG {
		// First-sight seeds land in the shared accounts table too, so every
		// replica's reserve sees an account this one just discovered.
		chOpts = append(chOpts, console.WithLedgerSink(func(ctx context.Context, accountID string, mintedMicro uint64) error {
			return stores.atomic.SetMinted(ctx, accountID, mintedMicro)
		}))
		// The account route and the balance stream read the shared row, so
		// two replicas never disagree on locked, pending or settled.
		if reader, ok := stores.atomic.(meter.AccountReader); ok {
			chOpts = append(chOpts, console.WithAccountReader(reader))
		}
	}
	// /chains and /attestation both describe the one chain the system
	// runs on. With no watcher the attestation producer reports nothing
	// (an empty row list hides the card) while /chains still names
	// Solana, so a configured-but-quiet deployment degrades quietly.
	chOpts = append(chOpts,
		console.WithChains([]console.ChainInfo{{
			ChainKey: 0,
			Name:     "Solana",
			RPC:      solCfg.RPC,
			Deposit:  solCfg.Treasury,
			Explorer: "https://explorer.solana.com",
		}}),
		console.WithAttestation(console.NewAttestHealth(func() console.WatcherHealth {
			if depositWatcher == nil {
				return console.WatcherHealth{}
			}
			h := depositWatcher.Health()
			return console.WatcherHealth{LastScan: h.LastScan, Head: h.Head, Err: h.Err}
		})),
	)
	ch := console.New(chOpts...)
	if settleClient != nil {
		ch.Minted = settleClient
	}
	// Re-derive every account's lock from the listing rows before the
	// first request: the write routes refresh as they land, but a listing
	// the indexer recorded (or a confirm that failed after the row was
	// written) never reaches them, and a restart must not trust whatever
	// figure the last process left behind.
	if err := ch.RebuildLocks(context.Background()); err != nil {
		log.Printf("slotfared: console: rebuild listing locks: %v", err)
	}
	root := http.NewServeMux()
	// handlerRoot is the handler the server serves; it stays the bare mux
	// while CORS is off and becomes the wrapped mux when origins are set.
	handlerRoot := http.Handler(root)
	// /health is liveness (process up, always 200) and /ready is readiness
	// (the store reachable); both are unauthenticated and answer probe
	// traffic: the exact patterns win over the "/" catch-all, so they never
	// reach the proxy.
	root.Handle("/health", healthHandler(stores.meter, 2*time.Second))
	root.Handle("/ready", readyHandler(stores.meter, 2*time.Second))
	// /.well-known/slotfare-receipt-key serves the receipt signer's public
	// identity unauthenticated - it is a public key, not a secret. It
	// answers 404 when receipts are disabled.
	root.Handle("/.well-known/slotfare-receipt-key", receiptKeyHandler(handler.Receipt))
	root.Handle("/console/", http.StripPrefix("/console", ch))
	root.Handle("/", handler)

	// SLOTFARE_CORS_ORIGINS is a comma-separated list of exact origins
	// allowed to call slotfared cross-origin - the console when it is deployed
	// on a different host than the API. Empty or unset keeps slotfared without
	// CORS headers, which is the same-origin and dev-proxy behavior of today.
	var corsOrigins []string
	for _, o := range strings.Split(os.Getenv("SLOTFARE_CORS_ORIGINS"), ",") {
		if o = strings.TrimSpace(o); o != "" {
			corsOrigins = append(corsOrigins, o)
		}
	}
	// methodsFor resolves a path to the HTTP methods that path actually
	// serves, so a preflight advertises only those. The console paths are
	// relative to the /console mount; the proxy prefixes match the way the
	// proxy itself does (a plain prefix).
	methodsFor := func(r *http.Request) []string {
		path := r.URL.Path
		switch {
		case path == "/health", path == "/ready":
			return []string{http.MethodGet}
		case path == "/.well-known/slotfare-receipt-key":
			return []string{http.MethodGet}
		case strings.HasPrefix(path, "/console/"):
			return console.AllowedMethods(strings.TrimPrefix(path, "/console"))
		case strings.HasPrefix(path, "/rpc"):
			return []string{http.MethodPost}
		}
		// Inference paths only exist when the service is enabled; when it
		// is not, they 404 through the proxy anyway and advertise nothing.
		if inferenceUpstream != "" && strings.HasPrefix(path, inferencePrefix) {
			switch strings.TrimPrefix(path, inferencePrefix) {
			case "/v1/models", "/health":
				return []string{http.MethodGet}
			case "/v1/chat/completions", "/v1/completions", "/v1/embeddings", "/v1/rerank":
				return []string{http.MethodPost}
			}
		}
		return nil
	}
	if len(corsOrigins) > 0 {
		handlerRoot = cors.Wrap(handlerRoot, corsOrigins, methodsFor)
		log.Printf("slotfared: CORS enabled for %d origin(s), first %s", len(corsOrigins), corsOrigins[0])
	}

	server := &http.Server{
		Addr:         listen,
		Handler:      handlerRoot,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  2 * time.Minute,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- server.ListenAndServe() }()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve: %w", err)
		}
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("slotfared: shutdown: %v", err)
		}
	}
	return nil
}

// startSettler starts the settlement loop when the Solana chain surface is
// configured (SLOTFARE_SOLANA_RPC); without it slotfared runs metering and
// flushing only, which keeps the offline demo path working. It returns the
// chain client so the console can read minted balances through it, nil when
// disabled. Credits mint from USDC deposits (the watcher writes SetMinted on
// the same authority); BalanceOf re-reads that authority so each pass
// converges. Every settle batch commits its usage Merkle root to Solana as
// a Memo program transaction and burns the covered spend in the local
// authority inside CommitRecord's store transaction.
func startSettler(ctx context.Context, cfg solana.Config, l *meter.Ledger, meterStore meter.Store, atomic meter.AtomicLedger, locker meter.Locker, interval time.Duration, threshold uint64) (*solana.Client, error) {
	if !cfg.Enabled() {
		log.Println("slotfared: settlement disabled: SLOTFARE_SOLANA_RPC not set")
		return nil, nil
	}
	balance, err := balanceFunc(atomic, l)
	if err != nil {
		return nil, err
	}
	client, err := solana.NewClient(cfg, balance)
	if err != nil {
		return nil, fmt.Errorf("settlement client: %w", err)
	}
	// Ledger-only integrity. Two rails exist during the port, so the one thing
	// an operator must never get is a daemon that *looks* like it is enforcing
	// the ledger while it is silently committing Memo transactions. The rail is
	// chosen by the config, not per request, so this is a startup check, not a
	// runtime branch.
	//
	// SLOTFARE_LEDGER_REQUIRED refuses the Memo rail outright: it is a hard
	// error to run without a program when it is set, so a deployment that means
	// to have on-chain enforcement cannot be handed one without it.
	if err := ledgerRequiredError(envOr("SLOTFARE_LEDGER_REQUIRED", "") == "1", client.LedgerEnabled()); err != nil {
		return nil, err
	}
	// When a program is configured, a config account must exist before the
	// settler can burn against it. Reading it once at startup turns a first
	// settle that fails on every account into one sentence naming the fix.
	if client.LedgerEnabled() {
		ledgerCfg, ok, err := client.LedgerConfigRead(ctx)
		if err != nil {
			return nil, fmt.Errorf("ledger program %s: %w", client.LedgerProgram(), err)
		}
		if !ok {
			return nil, fmt.Errorf("ledger program %s has no config account; run `slotfared ledger-init` before starting", client.LedgerProgram())
		}
		log.Printf("slotfared: settlement commits through the ledger program %s (authority %s)", client.LedgerProgram(), ledgerCfg.Authority)
	}
	settler := meter.NewSettler(l, client, interval, threshold)
	settler.Atomic = atomic
	settler.Locker = locker
	settler.Attestor = client
	settler.BatchStore = meterStore
	go func() {
		if err := settler.Run(ctx, func(err error) { log.Printf("slotfared: settle: %v", err) }); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("slotfared: settler stopped: %v", err)
		}
	}()
	return client, nil
}

// ledgerRequiredError is the ledger-only guard, as a pure function so the
// decision is testable without a cluster: with SLOTFARE_LEDGER_REQUIRED set,
// running without a configured program is an error, because the operator asked
// for on-chain enforcement and the Memo rail does not provide it.
func ledgerRequiredError(required, enabled bool) error {
	if required && !enabled {
		return errors.New("SLOTFARE_LEDGER_REQUIRED=1 but SLOTFARE_LEDGER_PROGRAM is unset; refusing to run the Memo-only settlement rail")
	}
	return nil
}

// balanceFunc returns the deposit-derived minted balance in whole credits,
// read from whichever side is the balance authority: the shared accounts
// table in Postgres mode, the in-memory ledger in sqlite mode. The chain
// holds no credit balances in this design, so nothing is dialed.
func balanceFunc(atomic meter.AtomicLedger, l *meter.Ledger) (solana.BalanceFunc, error) {
	if atomic != nil {
		reader, ok := any(atomic).(meter.AccountReader)
		if !ok {
			return nil, errors.New("settlement: balance authority does not expose Account")
		}
		return func(ctx context.Context, id string) (uint64, error) {
			a, err := reader.Account(ctx, id)
			if err != nil {
				return 0, err
			}
			return a.Minted / meter.MicroCreditsPerCredit, nil
		}, nil
	}
	return func(_ context.Context, id string) (uint64, error) {
		return l.Snapshot()[id].Minted / meter.MicroCreditsPerCredit, nil
	}, nil
}

// receiptKeyHandler serves the receipt signer's public identity at
// /.well-known/slotfare-receipt-key, unauthenticated: it is a public key, not
// a secret. GET only; anything else is 405. When receipts are disabled
// (nil signer) the path answers 404 - the deployment simply does not sign.
func receiptKeyHandler(s *receipt.Signer) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if s == nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		body := fmt.Sprintf(`{"alg":"ed25519","address":"%s"}`, s.Address())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, body)
	})
}

// envOr returns the value of the environment variable when set and
// non-empty, otherwise the default.
func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// advertisedBaseURL computes the OpenAI-compatible base URL a key holder
// curls for inference: <root>/<prefix>/v1. The root is publicURL when set
// (it must parse as an http/https URL with a host); otherwise it is derived
// from the listen address with an http scheme. A bare listen value is
// treated as a port on 127.0.0.1; an empty one as the default port.
func advertisedBaseURL(publicURL, listen, prefix string) (string, error) {
	if publicURL != "" {
		u, err := url.Parse(publicURL)
		if err != nil {
			return "", fmt.Errorf("SLOTFARE_PUBLIC_URL %q: %w", publicURL, err)
		}
		if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return "", fmt.Errorf("SLOTFARE_PUBLIC_URL %q must be an http or https URL with a host", publicURL)
		}
		return strings.TrimRight(publicURL, "/") + prefix + "/v1", nil
	}
	host, port := "127.0.0.1", "8443"
	if i := strings.LastIndex(listen, ":"); i >= 0 {
		host = listen[:i]
		port = listen[i+1:]
	} else if listen != "" {
		port = listen
	}
	// A bracketed IPv6 listen host keeps its address but drops the
	// brackets, so the derived URL stays plain text in logs and docs.
	host = strings.TrimPrefix(strings.TrimSuffix(host, "]"), "[")
	return "http://" + host + ":" + port + prefix + "/v1", nil
}
