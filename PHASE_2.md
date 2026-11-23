# Phase 2 technical specification

## 1. Scope

Phase 2 implements the shared execution primitives in Go. [Phase 1](PHASE_1.md) defines financial meaning. This document records supported profiles and implementation decisions. The demo exists to exercise the primitives together. Agent simulation, production protocol adapters and research benchmarking belong to subsequent gates.

## 2. Deterministic random stream

`prng.New(uint64)` expands the given seed, including zero, into four 64-bit state words using successive SplitMix64 outputs. `Uint64` applies the reference xoshiro256** transition in modulo-2^64 arithmetic. The algorithm ID is `xoshiro256ss-splitmix64-v1`. `State` serializes the words in index order as 32 little-endian bytes. Persist the algorithm ID with the state. `Restore` rejects the absorbing all-zero state.

`Below(n)` returns an unbiased integer in [0, n). Let W = 2^64 and r = W mod n. The algorithm discards words less than r. The W − r accepted words span a whole number of residue cycles modulo n, so every residue is equally likely under a uniform input word. A zero bound is an error. Rejection draws advance the stream and are part of the deterministic contract.

Use one stream per logical consumer and consume it in a predetermined order. The zero Go value has all-zero state, so initialize with `New` or `Restore`. The stream is for simulation. Its outputs are predictable to a holder of the seed or state.

## 3. Time contract

`simclock.New(start,end)` accepts an inclusive UTC horizon. `AdvanceTo` and `AdvanceElapsed` move forward, allow equality and reject movement outside that horizon without changing the instant. Use `AdvanceTo` for horizons larger than a Go duration. No host clock enters the simulation.

Use `Civil.Resolve` before scheduling a local time. It verifies Gregorian date and time components, handles leap days, rejects leap seconds under Go's time model, and constructs candidate UTC instants from local offsets on both sides of nearby timezone transitions. A spring gap returns an error. In a fall overlap choose Earlier or Later explicitly, or RejectAmbiguous. Elapsed time and wall-clock scheduling have separate APIs. UTC and the bundled Europe/London and America/New_York zones are supported. Load these through `Zone.Location` to preserve the bundled rules. Avoid host timezone lookups in a reproducible run.

`Calendar.DateAt` uses the market's zone. `IsBusinessDay` rejects dates outside the profile's supported years, including unsupported dates encountered during rollover. `Following` returns the date itself when it is open or advances over closed days. Calendar checks operate on dates and carry no payment-rail cutoff times.

| Market | Inclusive coverage | Basis |
|---|---:|---|
| England and Wales | 2019–2028 | GOV.UK snapshot including substitutes and published special holidays |
| Scotland | 2019–2028 | GOV.UK regional snapshot |
| Northern Ireland | 2019–2028 | GOV.UK regional snapshot |
| US Federal Reserve Banks | 2022–2030 | Recurring rules for Reserve Banks, including Juneteenth. A Sunday fixed holiday closes Monday. A Saturday fixed holiday leaves the preceding Friday open. |

US rules cover ordinary recurring holidays and weekends. Exceptional closures and individual settlement services can use different schedules. UK dates reflect the downloaded snapshot and should be versioned when updated.

### Reference data

The holiday dates are derived from the [GOV.UK bank holidays feed](https://www.gov.uk/bank-holidays.json), retrieved 23 November 2026. US rules follow the [Federal Reserve Financial Services holiday schedule](https://www.frbservices.org/about/holiday-schedules/). Timezone binaries use IANA tzdata version 2026c as identified by the source system's `tzdata.zi`. The loader uses [Go's LoadLocationFromTZData](https://pkg.go.dev/time#LoadLocationFromTZData). The random stream follows [Blackman and Vigna's reference implementation](https://prng.di.unimi.it/xoshiro256starstar.c).

| Bundled file | SHA-256 |
|---|---|
| London | c85495070dca42687df6a1c3ee780a27cbcb82f1844750ea6f642833a44d29b4 |
| New_York | e9ed07d7bee0c76a9d442d091ef1f01668fee7c4f26014c0a868b19fe6c18a95 |
| england-and-wales.txt | a4efa5d688e0cc43f5c1dc53a0b0d05656c6046354304dab4655bdf10d027fbe |
| northern-ireland.txt | 05e6fb65f8cd772b05ee907cc18645ad48d8d939f09b5f3ffcff48b08c6d95d7 |
| scotland.txt | d51a6bff104772fc930711a67fed79882cdbfa8c209a8747f8e35baf838380a1 |

## 4. Ledger API and semantics

Construct one ledger per accounting book with `ledger.New`. Pass the selected currency catalog, synthetic counterparties, fixed FX rates and optional start of the open accounting window. The engine copies these inputs. For each `Apply`, supply one operation, a source-scoped request key and a simulated recording instant. See the complete example in [cmd/ssfd-demo/main.go](cmd/ssfd-demo/main.go). Callers retain ownership of command pointers and must keep them stable during the call.

| Operation | Required effect |
|---|---|
| Open account | Establish immutable class, currency, owner and purpose. Create an Active event with zero opening balance. |
| Change account | Match current state and allowed transition. Dormancy uses configured elapsed inactivity. Closure requires zero booked balance, zero reserves and no holds. |
| Change hold | Place or release an identified directional hold. Holds prevent authorization and outgoing posting legs. Frozen accounts block all postings in this profile. |
| Open transaction | Record Pending intent and validate synthetic parties and any exact FX quote. |
| Authorize | Reserve customer funding from a compatible owned asset or liability account. Trusted internal bookings by an institutional synthetic debtor can use zero reserve. |
| Clear | Require matching authorization and intent, consume the reserve, and install a balanced journal. |
| Cancel or decline | Follow the allowed state transition. Cancellation releases any attributable reserve without posting. |
| Dispute or resolve | Open or resolve a workflow dispute without booking the original twice. |
| Reverse or adjust | Check original reference, leg direction, amount and applicable time bounds. A full reversal uses the original ordered inverse. A partial adjustment consumes remaining reversible amounts. A terminal unwind uses the full remainder. |

For customer transfers, the posting direction that reduces normal-side balance is outgoing. An enforced floor on available normal-side funds applies to all postings affecting the account. Also protect other outstanding reserves even when the account floor is disabled. A negative configured floor supports controlled overdrafts. Incoming credits and eligible servicing can post on a Dormant account under the lifecycle policy. Trust belongs to the simulation caller, which controls institution, account-state, hold and servicing inputs. This is an in-process API with no user authentication layer.

Ordinary customer same-currency transfers use a funding leg and compatible normal-side beneficiary legs. Customer FX additionally uses FX position accounts and targets a deposit liability. More general compound accounting, including opening funding, can use the trusted institutional path. That path validates intended gross amounts by currency while accepting explicit documented posting instructions from its trusted caller. Independent cross-book bookings are possible using separate ledgers. The implementation provides no atomic operation spanning multiple books. Corrections operate on original account legs. A product-specific correction using substitute servicing accounts, a period-close workflow, and non-monetary obligations beyond modeled holds and reserves require policies outside this profile.

### Atomic admission and replay

Normalize recorded and effective timestamps to UTC. Canonical request bytes are Go JSON of the fixed versioned command struct, whose operations and posting fields contain ordered lists and no maps. Compare the complete bytes for accepted key reuse, including the simulated command time and enabled optimistic precondition. An identical retry succeeds before timestamp and sequence guards, since it contributes no new history. Changed content with an accepted key fails. A rejected command leaves the key unbound. Retrying an accepted command requires its original timestamp and body.

Validate all cross-record checks before publication. The book-wide writer lock covers the validation and publication of the journal, projected balances, reservations, remaining adjustment limits, graph incidences, lifecycle events and idempotency receipt. Readers use the same lock in shared mode. The account view reports numeric availability before applying directional or frozen-state restrictions and exposes hold count and lifecycle state independently. An optional expected sequence makes stale commands fail. Lock acquisition determines the order of distinct concurrent commands. Reproducible generation must choose an explicit order before admission. Order equal-time inputs by stable producer and input IDs, and pin that ordering policy with the seed and configuration.

Commits are atomic in process. Durability across crashes would require an external persisted command journal or storage-backed commit mechanism. The package keeps accepted canonical payload bytes in memory for collision-free comparison. It retains posted legs and adjacency for auditability, so memory grows with accepted operations. Throughput and memory costs remain research measurements.

### Graph and audit

The graph has account and journal nodes. A credit points from its account to its journal and a debit points from its journal to its account. Parallel incidences are retained using one-based posting positions. `Edges` gives a stable account incidence list. `Snapshot` returns a copy of a committed prefix, with account and transaction IDs sorted and journals and events kept in commit order. Ownership of returned slices and quotes belongs to callers.

`Audit` recomputes debit-positive balances from the ordered journal stream with arbitrary-precision arithmetic, checks each exact journal balance, compares reconstructed balances and graph incidences to the live projections, and outputs a currency-sorted trial balance. It holds a consistent read lock during verification. A trial-balance zero is a bookkeeping invariant. It does not establish business realism or semantic correctness by itself.

## 5. Verification and research alignment

| Question | Evidence included |
|---|---|
| Does the random algorithm match the reference transition? | Published reference state vector, seed expansion fixture, bounded draws and checkpoint replay |
| Does the clock safely cross civil-time boundaries? | UK and US DST gaps and folds, leap-day acceptance and rejection, multi-year horizon, holiday substitutes, UK regional differences and differing US weekend rules |
| Do transactions conserve booked value? | Independent journal replay, per-currency audits, balances exceeding signed 64-bit range, and FX settlement with later reversal at original amounts |
| Do duplicate, stale and invalid commands preserve state? | 32 concurrent identical retries, same-key conflicts, stale sequence rejection, frozen/held or unauthorized clearing, over-refund rejection and snapshot integrity |
| Does the combined fixture remain identical? | Golden digest for seed 42 plus byte comparison between native Linux amd64 and QEMU-emulated Linux arm64 executions |

Verification used Go 1.24.13. All packages passed the native race-enabled suite and vet. The Linux arm64 suite passed under QEMU 7.2. Seed 42 yielded byte-identical demo output on the two targets. Seed 43 produced SHA-256 `dda47bbb9ca84db5dea39033b34ee4838a23b4a5b80b8e59b6fad0169bc046ae`. Linux 386 was unverified because the host rejected native 32-bit execution and the 386 emulator failed during Go runtime startup.

The proposal describes three research workstreams. The six implementation gates remain the delivery sequence. The Phase 1 and Phase 2 artifacts establish the core construction evidence. The behavioral and serializer gates will provide the workloads for later empirical evaluation. The proposed 10 million transaction invariant study is a research target beyond this fixture, while the original 1 million or more performance benchmark is a separate measurement target. Comparator ledger and optional SWIFT MT/gRPC scope can be selected during the relevant future gate. The original Phase 5 protocol families remain the committed starting scope.
