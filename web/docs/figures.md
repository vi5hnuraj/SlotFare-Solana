# Figure sourcing audit

Every number rendered on a slotfare-console screen has a source. Each figure is
either **derived** from live account/usage data (a computed over signals),
**dropped** (removed because it could not be sourced), or **labeled** (kept
because it is a declared constant, a structural fact, or an explicitly hedged
estimate). Nothing on screen is a hardcoded figure that outlives its data.

The two pure modules that back the derived figures are `src/app/core/burn.ts`
(burn rate, runway, period stats) and `src/app/core/relative-time.ts`
(relative times). Both are unit-tested against hand-computed values.

---

## Connect (`connect.component.html`)

| Figure | Source | Decision |
|---|---|---|
| `{{ walletSvc.providers().length }} provider(s)` | `WalletService.providers()` — real EIP-6963 wallet list | labeled (live count of detected wallets; singular at 1, plural otherwise) |

No other figures. The lede copy carries no numbers.

## Overview (`overview.component.html`)

| Figure | Source | Decision |
|---|---|---|
| Credit balance `{{ store.balanceText() }}` / `{{ store.balanceUsd() }}` | `ConsoleStore.balance` → `creditText()` / `usd()` | derived |
| Locked line `{{ store.lockedText() }} credits locked by open sell listings` (rendered only when `locked > 0`) | `ConsoleStore.locked` → `creditText()` | derived |
| Locked card: value `{{ creditText(store.locked()) }}`, sub `{{ usd(store.locked()) }} · held by {{ openListingsCount() }} open sell order(s)` (card rendered only when `locked > 0`) | `locked`, `usd(locked)`, `openListingsCount()` computed over `listings()` | derived |
| `About {{ store.promptTokens() }} prompt tokens … {{ store.completionTokens() }} completion tokens` | `mTokens(balance, RATE.*)` | derived |
| Spent · last 24 h value `{{ creditText(store.spend24h()) }}` | `spendLast24h(usage, now)` (burn.ts) | derived |
| Spent · last 24 h sub `{{ usd(store.spend24h()) }}` · `{{ creditText(store.creditsPerHour()) }} credits/hour` | `usd(spendLast24h)` and `creditsPerHour(usage, now)` | derived |
| Burn rate · 7 day avg value `{{ creditText(burn) }}` | `burnPerDay(usage, now)` — 7-day window, stated in the label | derived |
| Burn rate sub `credits per day · {{ usd(burn) }}` | `usd(burn7d())` | derived |
| Burn rate when no usage: value `—`, sub `Not enough usage yet` | `burn7d() === null` gate | derived (undefined, not 0/∞/NaN) |
| Runway value `{{ r.toFixed(1) }} days` | `runwayDays(balance, burn7d())` | derived |
| Runway sub `empty around {{ store.emptyDateText() }} at this rate` | `emptyDateText()` = `now + runwayDays * DAY_MS`, Intl `en-US` short month+day | derived |
| Runway when no usage: value `—`, sub `Not enough usage yet` | `runwayDays() === null` gate | derived (undefined, not ∞/NaN) |
| Recent requests table: Time, Key, Prompt, Cached, Completion, Credits | `store.usage()` rows via `num()` (tokens) / `creditText()` (credits) | derived |
| Funding-in-progress card: `{{ creditText(store.purchase().credits) }}`, `{{ store.purchase().amount.toFixed(2) }} {{ store.purchase().token }}` | `store.purchase()` view over `fundingStatus()` / configured amount | derived |
| Endpoint card: Model `{{ modelId }}`, Context window `{{ contextWindow }}` | `MODEL_ID`, `num(CONTEXT_WINDOW)` (pricing.ts) | labeled (declared constants) |
| Active keys count `{{ store.activeKeys().length }}` | `activeKeys()` computed | derived |
| Active keys list `{{ relTime(k.lastUsedAt) }}` | `relativeTime(lastUsedAt)` (relative-time.ts) | derived |
| `Rate 1,000 credits = $1.00` | `CREDITS_PER_USD` (pricing.ts) | labeled (rate constant) |

## Add credits (`add-credits.component.html`)

| Figure | Source | Decision |
|---|---|---|
| Preset amounts `${{ p }}` / `{{ creditText(p * 1000) }} credits` | `PRESET_AMOUNTS` × `CREDITS_PER_USD` | labeled (presets) / derived |
| Custom amount min/max hint `Minimum $5. Maximum $50,000` | mirrors `amountValid()` bounds in console-store.ts | labeled (input bounds) |
| `What this buys`: `{{ creditText(store.creditsToBuy()) }}`, `{{ store.buyPrompt() }}`, `{{ store.buyCompletion() }}` | `creditsToBuy()`, `mTokens(...)` | derived |
| Review: You pay `{{ store.payText() }}`, Rate `1,000 credits / $1.00`, Credits minted `{{ creditText(store.creditsToBuy()) }}` | `payText()`, `CREDITS_PER_USD`, `creditsToBuy()` | derived / labeled |
| Review: Slotfare fee `0.00` | structural — no fee is taken | labeled |
| ~~Est. Ethereum gas `~$0.42`~~ | **dropped** — no chain access is wired into this screen; estimating adds out-of-scope surface | dropped |
| Review: New balance `{{ creditText(store.newBalance()) }}` | `newBalance()` = balance + creditsToBuy | derived |
| Notice `typically 4 to 7 minutes` | hedged by "typically" | labeled (estimate) |

## Funding trace (`funding-trace.component.html`)

| Figure | Source | Decision |
|---|---|---|
| Card head `Purchase {{ s.purchaseId }}` (or `No purchase in flight`) | `fundingStatus().purchaseId` | derived |
| Step label `Step X of 5` / `Complete` | `stepLabel()` over `stageIndex(fundingStatus)`; 5 is the trace stage count | derived / labeled (structural) |
| Elapsed `{{ store.elapsedText() }}` | `clock((Date.now() - startedAt)/1000)` from a real `startedAt` | derived |
| Per-step block numbers, values, confirmation counts | `buildSteps(fundingStatus)` (trace.ts) over on-chain facts | derived |
| Purchase card: Paid, Credits, Value, Started | `store.purchase()` view | derived |
| `Minted on Solana at slot {{ num(store.purchase().ccBlock) }}` | `purchase().ccBlock` | derived |
| From/Credit account `{{ store.address }}` | `address()` computed | derived |

## API keys (`api-keys.component.html`)

| Figure | Source | Decision |
|---|---|---|
| Key handle `{{ k.display }}` | `keyDisplay(id)` = `slf_<id>` | derived |
| Label `{{ k.label }}`, Created `{{ k.created }}` | key fields | derived |
| Last used `{{ relTime(k.lastUsedAt) }}` | `relativeTime(lastUsedAt)` (relative-time.ts); muted when `lastUsedAt === null` | derived |
| Requests `{{ num(k.requests) }}` | `k.requestCount` via `num()` | derived |
| Base URL `{{ baseUrl() }}` copy row | `store.endpointBaseUrl()` — wire-advertised value, or page origin + `/v1` when the wire carries none | derived (payload) |
| First-request curl example, `Bearer $SLOTFARE_API_KEY` with per-kind body (messages + max_tokens / input array / query + documents) | `buildCurl(baseUrl(), selected() ?? placeholder)` — wire base URL, selected model's real id; `<model-id>` placeholder with a chat-shaped body while no catalog has loaded; the header references `$SLOTFARE_API_KEY`, exported once from one of your keys (double-quoted header, single-quoted JSON body) | derived (payload) + labeled (setup) |
| Micro-credit metering note (`x-slotfare-credits-charged`, `x-slotfare-balance`, 10,000 = 1 credit; streamed responses omit the charge header) | static descriptive text | labeled |

## Usage (`usage.component.html`)

| Figure | Source | Decision |
|---|---|---|
| `{{ store.usageDays() }}-day total` value `{{ creditText(s.total) }}` / `{{ usd(s.total) }}` | `periodStats(DAILY_SPEND.slice(-usageDays))` | derived |
| Daily mean `{{ creditText(s.mean) }}` / `{{ usd(s.mean) }} per day` | `periodStats(...).mean` | derived |
| Peak day `{{ creditText(s.peakValue) }}` / `{{ peakDateText() }}` | `periodStats(...).peakValue` + Intl weekday/day/month from `dayZero + peakIndex*DAY_MS` | derived |
| Requests `{{ num(r.total) }}` / `{{ creditText(creditsPerRequest()) }} credits each, mean` | `periodStats(DAILY_REQUESTS.slice(-usageDays))` and spend-total ÷ request-total | derived |
| Stats panel when range empty: `—` / `No usage in range` | `stats() === null` gate | derived (no NaN) |
| Chart bars / mean line / axis labels | `SpendChartComponent` over the sliced series | derived |
| Legend `{{ store.usageDays() }}-day mean` | `usageDays()` | derived |
| Legend `Last day is partial` | static, true for any trailing partial day | labeled |
| Request log table cells | `store.usage()` rows via `num()` (tokens) / `creditText()` (credits) | derived |
| Log totals row `{{ num(...) }}` / `{{ creditText(...) }}` / `{{ usd(...) }}` | `store.usageTotals()` | derived |
| `{{ store.usageCountText() }}` | `usageCountText()` = `usage().length` | derived |

## Models (`models.component.html`, `model-detail.component.html`)

| Figure | Source | Decision |
|---|---|---|
| Model rows: name, id, vendor tag, headline pricing `input {{ creditText(m.pricing.uncachedInput) }} credits / 1M tokens`, description, context `{{ num(m.contextLength) }} tokens context` | `models()` wire rows; absent fields render nothing | derived (payload) |
| Vendor filter chips with counts, `other` grouping; search box; composed filtering | `deriveVendors`, `applyFilters` (model-filters.ts) | derived (payload) |
| Detail head: name, id (copyable), kind badge, serving-path badge | routed model row; kind/serving path shown via documented absent-means defaults (`modelKind`, `servingPath ?? 'standard'`) | derived (payload) + labeled (defaults) |
| Details card: Kind, Serving path, Context window `{{ contextShort(contextLength) }}` (humanized k/M), Mixture of experts Yes/No from `moe`, Parameters, capability badges over the six wire slugs (text, image, video, audio, vision, function-calling) | routed model row; context/moe/parameters/capabilities only when present; labels via `CAPABILITY_LABELS` | derived (payload) |
| Two-column split at the desktop breakpoint: Details and Pricing in a left track, the Example card in a right track; stacked in today's order below it | `.split` / `.cols-1` driven by `BreakpointService` (`bp() !== 'desktop'`) | labeled (layout) |
| Pricing table: Uncached input / Cached input / Output in credits per 1M tokens | `creditText(p.uncachedInput \| cachedInput \| output)` on the wire per-1M rates | derived (payload) |
| Example card: Base URL copy row, curl example with its own block-level copy button, `$SLOTFARE_API_KEY` header with keys-screen link | `endpointBaseUrl()` and `buildCurl(baseUrl(), model)` — wire base URL, model's real id, kind-fitting body; the header references `$SLOTFARE_API_KEY`, exported once from a key from the API keys screen | derived (payload) + labeled (setup) |

## Sell (`sell.component.html`)

| Figure | Source | Decision |
|---|---|
| Card head `{{ creditText(store.balance()) }} spendable · {{ creditText(store.locked()) }} locked` | `balance`, `locked` signals → `creditText()` | derived |
| Credits-to-list hint `up to the {{ creditText(store.balance()) }} that are spendable` | `balance` → `creditText()` | derived |
| Pay token choices `{{ t.symbol }}` | pay-token list, USDC only (the market's one token) | labeled (deployment config) |
| Review: Credits listed `{{ creditText(credits()) }}` | form input via `creditText()` | derived (user input) |
| Review: Price `{{ dec2(price()) }} {{ token() }}` | form input via `dec2()` | derived (user input) |
| Review: Spendable after `{{ creditText(spendableAfter()) }}` | `spendableAfter()` = max(0, balance − credits) | derived |
| Refusal line `Only {{ creditText(store.balance()) }} credits are spendable; {{ creditText(store.locked()) }} are locked…` | `balance`, `locked` → `creditText()` | derived |
| Pre-wallet contract paragraph (lock now, unlock on cancel, transfer after maturation) | static copy, no figures | labeled |
| My listings table: State pill, Credits `{{ creditText(l.credits) }}`, Price `{{ listingPriceCell(l) }}`, Matured/Maturing | `store.listings()` rows; maturity is the server's `matured` boolean | derived |
| Notice `first listing with {{ token() }}` | chosen token symbol | labeled |

## Shell rail & topbar (`shell.component.html`)

| Figure | Source | Decision |
|---|---|---|
| Rail balance `{{ store.balanceText() }}` / `{{ store.balanceUsd() }}` | `balance` → `creditText()` / `usd()` | derived |
| Rail locked line `{{ store.lockedText() }} locked` (rendered only when `locked > 0`) | `locked` → `creditText()` | derived |
| Rail `≈ {{ store.promptTokens() }} prompt tokens` | `mTokens(balance, RATE.prompt)` | derived |
| Topbar title | static screen name | labeled |
| Topbar subtitle (overview) `Credit account {{ store.address() }}` | `address()` computed | derived |
| Topbar subtitle (funding) `Purchase {{ id }} · {{ chainName or "source chain" }}` | `routedPurchase()` + `fundingStatus()`; falls back to `{{ n }} transactions` with no purchase | derived |
| Topbar subtitle (keys) `{{ active }} active · {{ revoked }} revoked` | `activeKeys().length` and `keys().length - active` | derived |
| Topbar subtitle (usage) `Last {{ store.usageDays() }} days` | `usageDays()` | derived |
| Topbar subtitle (sell) `{{ openListingsCount() }} open · {{ creditText(locked()) }} credits locked` | `openListingsCount()`, `locked()` → `creditText()` | derived |
| Wallet chip `{{ store.address }}` | `address()` computed | derived |

## Key modal chrome (`key-modal.component.html`)

| Figure | Source | Decision |
|---|---|---|
| Kept prefix `{{ prefix() }}` | `plaintext.slice(0, 13)` | derived |

## Trade detail (`trade-detail.component.html`)

| Figure | Source | Decision |
|---|---|---|
| Status pill `Sold` / `Cancelled` | `listing().state` | derived |
| Card head `Listing {{ l.id }}` | the routed listing row's id | derived |
| Credits `{{ credits(l) }}` | `l.credits` → `creditText()` | derived |
| Price `{{ price(l) }}` | `listingPrice(l)` (raw units scaled by pay-token decimals) | derived |
| Listed at `{{ when(l.createdAt) }}` | `l.createdAt` via `when()` | derived |
| Listing transaction hash | `short(l.listedTx)` | derived |
| Listing block `{{ blockText(l.listedBlock) }}` | `l.listedBlock` → `num()`; dash when absent | derived |
| Matured `Yes`/`No` | the server's `l.matured` boolean | derived |
| Maturation deadline `{{ maturityDeadline(l) }}` | `l.maturesAt` unix seconds → ISO via `when()`; shown only when present | derived |
| Sale transaction / block / sold at | `l.soldTx`, `l.soldBlock` → `num()`, `l.soldAt` via `when()` | derived |
| Cancel transaction / block / cancelled at | `l.cancelledTx`, `l.cancelledBlock` → `num()`, `l.cancelledAt` via `when()` | derived |
| Outcome (buying sold) `Credits bought: +{{ creditText(l.credits) }}` | `l.credits` → `creditText()` | derived |
| Outcome (selling sold) `Proceeds received: {{ price(l) }}` | `listingPrice(l)` | derived |
| Outcome (selling cancelled) `Credits released: {{ creditText(l.credits) }} back to your balance` | `l.credits` → `creditText()` | derived |
| Outcome (buying cancelled) `No credits moved` | static copy, no figures | labeled |
| Explorer links (`Solana explorer · transaction`) | `ccTxUrl(l.listedTx)` / `ccTxUrl(l.soldTx)` / `ccTxUrl(l.cancelledTx)` — D56 chain-driven rule, one place | derived |
| Notice copy (public and verifiable on Solana) | static copy, no figures | labeled |

No other figures.
