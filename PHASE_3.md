# Phase 3 behavioral simulation specification

## 1. Entry points and reproducibility

Use `agents.Run(agents.Config{Seed: 42, StartYear: 2024, Years: 1})`. Each call creates a fresh synthetic bank book. The interval includes the whole start year and excludes midnight at the start of the year after the last simulated year. The England and Wales calendar bundled in Phase 2 bounds supported local dates to 2019–2028. The returned result includes the version, seed, PRNG algorithm, civil horizon, actor account mapping, ordered activity outcomes, outstanding loan positions, ledger snapshot and final per-currency audit.

The root xoshiro256** stream expands the numeric seed. Six successive root draws seed six separate actor-local stream instances in actor creation order. `prng.RNG` is an alias for the existing `prng.Stream`; both expose the Phase 2 stream and checkpoint methods. All stochastic decisions and size draws use `RNG.Below`. Pairing a history with the module version and calendar data is necessary because changes to the rule order, probabilities or draws alter replay. Every timestamp comes from the virtual clock. Ordered integer IDs are assigned at creation. Concurrent callers can run separate simulations because each run owns its book and streams.

## 2. Actors and accounts

This version models 12 private individuals and businesses in a single synthetic bank book. The bank is a separate institutional counterparty.

| Actors | Count | Owned deposit liability accounts |
|---|---:|---|
| Salaried retail customers | 6 | GBP current and savings |
| UK small businesses | 2 | GBP operating and reserve |
| US small business | 1 | GBP operating and reserve, USD foreign settlement |
| Landlord | 1 | GBP rental receipts and property reserve |
| Utility and subscription providers | 2 | GBP collection and treasury, one actor per provider type |

The bank supplies cash, GBP and USD FX position accounts, a loan collection liability, an interest income account and an asset account per loan. Opening cash is booked using paired asset and deposit legs. Two UK businesses receive external turnover credits and pay employees from their own operating cash. Both have domestic card receipts. The US merchant receives USD on the cross-border channel. Four retail actors pay rent. A separate retail actor finances a property purchase, with proceeds paid directly to the landlord as synthetic seller. One UK business borrows for working capital and receives the principal in its operating account. This is a calibrated synthetic scenario with assumed parameters. Its transaction counts and amounts are reproducible evidence for the software, and require external calibration for empirical claims about customer behavior.

## 3. Monthly schedule

Monthly obligations resolve to the following business day if the nominal date is closed. The engine resolves scheduled events at 09:00 Europe/London. It visits all local dates in ascending order and runs daily activity at 18:00 local time, including weekends. For events rolled onto the same date, nominal month and rule order break ties, followed by stable actor order.

| Nominal day | Operation |
|---:|---|
| 1 | Four rent direct debits to the landlord |
| 5 | Utility and subscription direct debits for all retail actors |
| 22 | Six funded payroll payments from the UK businesses |
| 24 | Retail mortgage collection and allocation |
| 25 | Business loan collection and allocation |
| 26 | Move 10% of each actor’s available GBP balance above £3,500 to savings, reserve or treasury |

## 4. Exact loan amortization

Both sample loans use equal principal and a fixed annual rate expressed in integer basis points. For principal P and N installments, regular principal is floor(P/N). The final installment pays the full remainder. Let B be opening outstanding minor units for a settlement period and let n/d be the day-count fraction. Interest is the nearest integer to `B × annualBasisPoints × n / (10000 × d)`, with ties to even. The implementation multiplies and divides in arbitrary-precision integers and rejects installments outside the positive int64 range. For ACT/365F, n is the count of civil Gregorian days between adjusted dates and d is 365, including when the interval crosses 29 February. For 30/360 ISDA, a start on day 31 becomes 30. An end on day 31 becomes 30 when the adjusted start is 30. The numerator is `360 × yearDelta + 30 × monthDelta + adjustedDayDelta` and d is 360. There is no special February month-end adjustment in this variant. See [OpenGamma Strata day-count definitions](https://strata.opengamma.io/day_counts/).

| Loan | Opening principal | Annual basis points | Installments | Day count |
|---|---:|---:|---:|---|
| Mortgage | £240,000 | 395 | 240 | ACT/365F |
| Business loan | £6,000 | 820 | 18 | 30/360 ISDA |

Accrual starts at origination and continues from the last adjusted settlement date. A due payment first uses the borrower’s ordinary authorization and clearing path, debiting the deposit liability and crediting the bank collection liability. A separate bank journal debits collection and credits the loan asset and accrued-for-period interest income. The adjustment to loan principal happens after both bookings succeed. The allocation is carried out immediately in the private simulation, with no intervening simulated activity. The two journal commits remain independently balanced. If a mandatory loan debit or allocation fails, the run returns an error instead of publishing a partial result. This profile models scheduled performing loans under funded payroll and an external-turnover assumption. Each run reconciles its outstanding amount with the corresponding ledger asset and verifies that collection ends at zero.

## 5. Daily activity

A retail actor starts Quiet. Each day, the engine selects a new state with the following transition weights per 100 draws.

| Current → next | Quiet | Routine | Active |
|---|---:|---:|---:|
| Quiet | 74 | 23 | 3 |
| Routine | 17 | 70 | 13 |
| Active | 9 | 35 | 56 |

In the resulting state, the actor selects one channel or remains idle. Card spend probabilities are 26%, 61% and 78% for Quiet, Routine and Active. Card spend amount bands are £3.00–£14.99, £3.00–£37.99, and £3.00–£72.99 respectively, with uniform minor-unit draws. A domestic merchant is selected uniformly. Each state assigns 9% to P2P payments of £2.00–£53.99 to an independently selected other retail actor and 2% to cross-border purchases of £4.00–£99.99 to the US settlement account. The remaining probability is idle. The FX rate is a fixed synthetic 5/4 USD per GBP and the bank books both currency partitions via the existing Phase 2 position-account legs. FX conversion uses the existing ties-to-even policy.

Insufficient available funds produce a recorded Declined outcome with no journal. Successful payments advance through Pending, Authorized and Cleared and use the same book commit boundary as Phase 2. Model channels share that lifecycle. Synthetic merchant card spending represents a settlement-sized account transfer tagged by activity, with no card network message timing modeled. Opening balances and loan disbursements use trusted institutional bookings through that same boundary.

## 6. Verification and expected run

The Go test suite checks Act/365F across leap day, 30/360 ISDA around February and month ends, both rounding tie cases, exact final principal, recurring activity counts and business-day times, full-year snapshot repeatability, a distinct second seed, insufficient-funds decline without a journal, and business loan maturity over two years. Phase 2’s golden fixture and original package tests remain in the bundle.

For seed 42 and the 2024 calendar year, the run produces 12 actors, 32 accounts, and 1,872 posted journals. They include 1,084 card spends, 197 P2P payments, 49 FX payments, 72 payroll transfers and 24 loan collections. All figures in the audit table are minor units.

| Currency | Debits | Credits | Difference |
|---|---:|---:|---:|
| GBP | 128780004 | 128780004 | 0 |
| USD | 324404 | 324404 | 0 |

The snapshot contains an ordered journal plus activity labels, account roles, and reproducibility metadata.

With Go 1.24.13 on Linux amd64, the race-enabled suite and vet passed. Two independent executions of the runner with `-seed 42 -snapshot` produced identical bytes. The full fixture SHA-256 is `902af1261de3309b0ea47c639545b6a16ca6fcf3cdf88979767aa085b7e5628e`.
