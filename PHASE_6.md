# SSFD-Engine Phase 6 evaluation protocol

Phase 6 adds a standalone evaluation command and preserves the Phase 5 engine, demo, serializer and sample interfaces. [EVALUATION_RESULTS.md](EVALUATION_RESULTS.md) records measured evidence from this implementation.

## Reproduction

Run from the extracted module with Go 1.22 or newer. Commands below pin concurrency to match the recorded experiment.

```sh
GOMAXPROCS=4 go run ./cmd/ssfd-eval -mode all > results.json
GOMAXPROCS=4 go run ./cmd/ssfd-eval -mode all -format markdown
GOMAXPROCS=4 go test ./eval -run '^$' -bench '^BenchmarkWorkloads$' -benchtime=1x -benchmem
GOMAXPROCS=4 go run ./cmd/ssfd-eval -mode benchmark -workload stress -stress-journals 1000000 -generation-only -repeats 2 > benchmark-million.json
GOMAXPROCS=4 go run ./cmd/ssfd-eval -mode mutation -seed 42 -mutation-seeds 12 -cases 192 -format markdown
GOMAXPROCS=4 go run ./cmd/ssfd-eval -mode mutation -mutant missing_idempotency,stale_authorization_balance_write
```

The runner supports `benchmark`, `mutation` and `all` modes. JSON includes per-repetition measurements, digests, trial balances, per-seed mutation results and first distinguishing scenarios. Markdown renders descriptive and paired comparison tables. Passing a mutant list enables only those faults, each in isolation. The normal demo remains a separate executable.

## Performance protocol

| Workload | Construction |
|---|---|
| Year | Calendar 2024, Phase 3 agents, default banking calendar and seeded behavioral activity |
| Decade | Calendar years 2019–2028, same agents and rules |
| Stress | A single book, four accounts, one posting-currency partition per journal, alternating GBP/USD, seeded deposit amounts, sequential timestamps and full transaction authorization and clearing |
| Million-journal supplement | Same stress workload, 1,000,000 transactions, generation and audit with a bounded-transient-memory canonical record digest |

Generation covers initialization, ledger commands, the engine's audit, and committed snapshot creation. For the behavioral cases this also includes activity and portfolio logic. The independent audit is timed separately. It replays every journal using arbitrary-precision integer sums, checks each journal's currency partition and verifies account-level balances against the snapshot. Two unbalanced journals cannot hide by cancelling in the final totals.

Three serializer stages are timed on each workload. They encode canonical snapshot JSON, official-version CAMT account statements, and CP037 signed-zoned flat batches. The timed stages include the serializer's projection and validation work. Digesting output, disk writes and official XML schema validation are outside those timings. The stress stream consists of synthetic institutional deposits, so credit-initiation and card exports would lack meaningful workloads for that fixture. The Phase 5 tests and archived examples still cover those adapters.

The command runs three sequential repetitions per base workload with seed 42. It calls GC before each stage, samples process-wide Go `runtime.MemStats.HeapAlloc` every 5 ms and at stage boundaries, and records cumulative `TotalAlloc` delta. Peak is the maximum observed sample, a lower bound on transient heap peaks. This metric excludes stack consumption and native overhead and differs from operating-system RSS. The sampler itself can perturb measurements. Median time and throughput describe this host and small repetition set. The distinct stress fixture is a scale workload for the ledger, so its throughput should be compared with other stress runs rather than treated as a scaling curve for the behavioral generator.

The runner compares SHA-256 for independently generated output bytes across repetitions and fails on a mismatch. The million-journal mode streams the canonical record sequence to SHA-256 instead of allocating each complete serializer output. It measures large in-memory generation and audit, while bulk serializer memory at that size is outside the measured profile.

## Mutation experimental design

The system under test is a purpose-built, two-account in-memory banking model. It accepts typed payment, authorization, clearing, hold, interest, FX and correction cases. It has no external banking or network dependency. The evaluator changes one rule at a time and compares each observable operation response with a clean reference SUT using the same case. A mutation is killed on the first difference in status, booked date, balances, remaining refundable principal, accrued amount, FX quote or posting count. All 13 mutants have unit tests with explicit expected clean responses and a distinguishing witness. This controlled oracle supports an internal comparison and has shared implementation with the mutants. The experiment is not a validation of an external ledger.

| Mutant | Injected defect |
|---|---|
| `fx_ties_round_up` | Round exact half ties up instead of ties to even |
| `missing_idempotency` | Post an identical command again |
| `leap_day_interest_omitted` | Skip the February 29 accrual |
| `dst_fold_collision` | Conflate distinct UTC instants sharing one local second |
| `overdraft_floor_exclusive` | Decline an amount that lands exactly on the allowed floor |
| `refund_cap_missing` | Accept refund principal over the remaining amount |
| `currency_mismatch_accepted` | Accept mismatched currencies on a domestic payment |
| `frozen_debit_bypass` | Clear a debit while the account is frozen |
| `clearing_without_authorization` | Post before authorization |
| `reversal_credit_omitted` | Debit the beneficiary on reversal and omit the payer credit |
| `cutoff_rounded_to_second` | Move a late-night entry across midnight by rounding |
| `settlement_credit_short_one` | Credit one minor unit less than the debit |
| `stale_authorization_balance_write` | Overwrite an interleaved debit using the balance captured at authorization |

Each seed generates three case corpora. The naive generator draws independent typed rows with random dates, amounts and currency codes, including some invalid domestic transfers. The happy-path corpus uses successful Phase 3 activity, including card authorization and clearing, actual FX principals and loan interest amounts. The chaos corpus uses that activity plus evidence-derived cases from Phase 4 journals and lifecycle histories. For rejected input, reinstate the observed scenario conditions in the mock SUT, such as the early clearance, duplicate, exact overdraft floor, or one-penny excess refund. These cases are normalized projections rather than direct command-log replay. The stale write case uses an actual later booking that intervened between authorization and delayed settlement. Its schedule is deterministic and models logical concurrency without depending on thread scheduling.

The base design uses seeds 42 through 53 and 192 cases per strategy per seed. The structured corpora use seeded shuffle within scenario types and round-robin sampling across types, so rare calendar cases receive representation in the same case budget. A case may have several operations. The report gives operation counts as well as equal case counts. Stratification and fault-family overlap are deliberate differences between the test-data strategies.

The seed-level mutation score is killed mutants divided by enabled mutants. The overall score averages equally across seeds, equivalently total kills divided by total mutant runs. Comparisons are paired by seed. The exact two-sided sign test counts strictly positive and negative score differences, drops ties and computes the binomial probability under 0.5. The report also gives the mean paired percentage-point difference. Its p-values are descriptive for this fixed model and closely related seeds. Neither the 13 selected faults nor the 12 seeds form a population sample of banking failures.


## References

- [Go runtime MemStats](https://pkg.go.dev/runtime#MemStats)
- [NIST sign test summary](https://www.itl.nist.gov/div898/software/dataplot/refman1/auxillar/signtest.htm)
