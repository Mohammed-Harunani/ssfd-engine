# Phase 4 deterministic fault injection

Phase 4 executes controlled synthetic scenarios at the typed, in-memory ledger boundary. The module exercises book-level validation and accounting outcomes. It has no network or live bank dependency.

## Run

```sh
go run ./cmd/ssfd-demo -seed 42 -start-year 2024 -years 1 -chaos-profile standard
go run ./cmd/ssfd-demo -seed 42 -start-year 2024 -years 1 -chaos-profile aggressive -snapshot > chaos-history.json
go test -race -cover ./...
go vet ./...
```

Normal runs continue through `agents.Run` and retain the Phase 3 output contract. An opt-in run calls `agents.RunWithChaos` and adds injector version, seed and an ordered counter array to the full result. With the CLI, the same user-supplied seed initializes the agent and injector independently. Programmatic callers can choose a separate chaos seed.

```go
result, err := agents.RunWithChaos(
    agents.Config{Seed: 42, StartYear: 2024, Years: 1},
    chaos.Config{Seed: 84, Weights: map[chaos.Fault]uint16{
        chaos.Ordering: 2000,
        chaos.Duplicate: 500,
        chaos.LeapDay: 10000,
    }},
)
```

Weights are probabilities per eligible opportunity in basis points. Zero or an omitted key disables a scenario. Invalid keys and weights greater than 10,000 are rejected. Draws and amounts use a separate xoshiro256** stream for each fault in a fixed order. Changing one fault's weight does not consume the other streams. Journal and transaction IDs can still differ if the set of selected faults changes. The profile version is `ssfd-chaos-v1`.

## Scenario matrix

| Fault | Standard weight (bps) | Eligibility and expected result |
|---|---:|---|
| `midnight_cutoff` | 1,000 | Every day. One posting journal at the final nanosecond of the preceding day if that instant remains inside the run, and two at exact local midnight. Distinct IDs and commit sequences preserve all valid commits. |
| `leap_day_accrual` | 10,000 | 29 February. Accrue one civil day using the existing ACT/365 Fixed loan calculation and book a second journal at the same instant. Opening synthetic principal of 365,000 pence at 500 annual basis points produces exactly 50 pence of interest. |
| `dst_overlap` | 10,000 | The local London 01:30 occurs twice on autumn clock-change day. Resolve both UTC instants explicitly and book one journal at each. A spring gap creates no overlap opportunity. |
| `early_clearing_and_delay` | 450 | Each day with a successor in the run. Reject clearing while Pending, authorize, then settle at 08:00 local on the following civil day. The last day drains the prior day's settlement. |
| `idempotency_replay` | 350 | Every day. Book one USD payment, submit an identical command and check its original receipt and unchanged balances. Reject changed content under the accepted key and reject a second clearing under a fresh command key. |
| `overdraft_boundary` | 400 | Every day. Settle to the allowed −500 pence balance. Reject a further penny and record a Declined transaction without a journal. Reverse the allowed charge to reset the account for the next date. |
| `partial_refund_and_chargeback` | 450 | Every day. Book payment, post a half-principal refund, open a dispute, reject a one-penny overchargeback, then unwind the remainder with the original booking reference. |
| `aml_freeze_and_reversal` | 250 | Every day. Authorize, apply a hold and freeze before clearing, verify rejection, release the restriction, clear, then reverse the original amounts. |

The aggressive profile sets all weights to 10,000. A fault is counted when selected on an eligible date. A scenario can book multiple journals, and a delayed settlement contributes its journal on the next date. `Rejected` counts expected blocked commands. `IdenticalRetries` counts accepted retries with the original receipt. Neither contributes a booked journal. `DelayedSettlements` counts delayed authorizations that eventually clear.

## System boundary and trade-offs

Each opt-in run creates nine dedicated accounts in the same book and two balanced GBP and USD opening journals. Typed account, party, transaction and journal identities use an isolated numeric namespace. Commands use source ID 2. This keeps the normal agent workload and its PRNG draw sequence reproducible while forcing both workloads to use the same commit validator, ordered history and reconciliation audit. Mechanisms model synthetic hold and freeze instructions supplied by the scenario. Regulatory decisioning and actual rail message adapters are separate responsibilities.

The injector processes calendar cases around midnight and then morning scenarios before regular 09:00 jobs. The delay model is one civil day, including weekends, and may span 23 or 25 real hours over DST changes. The user of the low-level injector must supply dates in successive order with one writer and discard the private run on an unexpected error. `RunWithChaos` enforces the workflow and publishes only a finished result after checking the ledger, loan subledger and the absence of any queued last settlement.

The CLI's standard activity counts track the behavioral scenario. Its book totals and audit include both workloads. Full snapshots expose the additional canonical history, including typed state events, journals and hold history. The two initialization journals are included in book totals and excluded from selected-fault counts.

## Verification

`chaos/chaos_test.go` runs the full 2024 aggressive scenario. It independently recomputes each journal's currency sums, checks GBP/USD final balances, inspects exact midnight and DST instants, verifies the interest booking, traces delayed settlement lifecycles, checks refund totals, and ensures no holds or reserves leak. A second test checks fractional probabilities, disabled faults, invalid configuration, reproducible output and the effect of changing the fault seed.

Seed 42 aggressive in 2024 produces 6,268 booked journals, including 4,394 fault journals and two fixture funding journals alongside 1,872 ordinary journals. It records 2,195 expected rejections, 366 successful same-receipt retries and 365 completed delayed settlements. Every journal examined by the test balances independently in each represented currency.

| Currency | Debit minor units | Credit minor units | Difference |
|---|---:|---:|---:|
| GBP | 138780054 | 138780054 | 0 |
| USD | 10324404 | 10324404 | 0 |

The no-chaos seed-42 full snapshot retains SHA-256 `902af1261de3309b0ea47c639545b6a16ca6fcf3cdf88979767aa085b7e5628e`. The retained Phase 2 regression runner uses `-phase2-fixture` and also preserves its prior digest.

Two separate aggressive seed-42 runs produced byte-identical full snapshots with SHA-256 `f05d6b4ee1823a285cbdc2e4cee4e972d277ac7276527bee339b1e41e61187a7`. The race-enabled suite and vet passed under Go 1.24.13 on Linux amd64.
