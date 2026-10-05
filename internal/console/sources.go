package console

// ChainInfo is one row of GET /console/chains: the metadata the console
// serves so it never hardcodes a chain name, id or explorer. Solana is the
// one chain Slotfare runs on, so a configured deployment serves exactly one
// row (chain key 0, the deposit chain itself); an unconfigured deployment
// serves 501.
//
// RPC is the cluster's public JSON-RPC endpoint. The console hands this URL
// to every visitor's browser so its wallet can build and send transactions
// against the chain it is on; operators MUST configure a public, keyless
// endpoint (nothing enforces that server-side beyond this comment). It is
// absent (omitempty) when unset.
//
// Deposit is the treasury's USDC token account - where a deposit goes.
// Explorer is the transaction-URL base the console links against.
type ChainInfo struct {
	ChainKey uint64 `json:"chainKey"`
	Name     string `json:"name"`
	ChainID  uint64 `json:"chainId"`
	RPC      string `json:"rpc,omitempty"` // public, keyless cluster RPC URL
	Deposit  string `json:"deposit"`       // treasury USDC token account (base58)
	Explorer string `json:"explorer"`      // explorer transaction URL base
}
