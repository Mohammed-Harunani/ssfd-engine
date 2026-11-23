# SSFD-Engine evaluation results

**Execution date** 23 November 2026. **Host** Intel Xeon Platinum 8573C, Linux amd64, Go 1.24.13, nine logical CPUs visible and GOMAXPROCS set to four. [Protocol and reproduction commands](PHASE_6.md).

**Raw evidence** [Three-workload benchmark and mutation runs](evaluation/results.json), [million-journal repetitions](evaluation/benchmark-million.json), and [Go benchmark output](evaluation/go-bench.txt). Tables are rendered from the recorded JSON using the same evaluation report formatter supplied with the project.

## Performance

go1.24.13 on linux/amd64, GOMAXPROCS 4, 9 logical CPUs visible, seed 42, 3 repetitions.

| Workload | Transactions | Journals | Postings | Generation ms (median [range]) | Journals/s | Postings/s | Peak heap MiB | Audit ms |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| year | 1872 | 1872 | 3866 | 34.58 [31.43–46.22] | 54137 | 111803 | 8.21 | 0.54 |
| decade | 18648 | 18648 | 38284 | 348.31 [298.37–533.09] | 53539 | 109915 | 88.28 | 6.36 |
| stress | 100000 | 100000 | 200000 | 1826.95 [1507.27–2185.64] | 54736 | 109472 | 455.48 | 35.79 |

All reported journal and account audits passed. Repeated output digest comparison passed: **true**.

| Workload | Currency | Trial-balance debits (minor units) | Credits (minor units) | Difference (minor units) |
|---|---|---:|---:|---:|
| year | GBP | 128780004 | 128780004 | 0 |
| year | USD | 324404 | 324404 | 0 |
| decade | GBP | 358350000 | 358350000 | 0 |
| decade | USD | 2902398 | 2902398 | 0 |
| stress | GBP | 2498329232 | 2498329232 | 0 |
| stress | USD | 2495732971 | 2495732971 | 0 |

| Workload | Encoder | Bytes | Median ms | Median MiB/s | Peak heap MiB |
|---|---|---:|---:|---:|---:|
| year | canonical-json | 1576167 | 20.55 | 73.16 | 5.86 |
| year | camt | 2343671 | 52.39 | 42.66 | 12.31 |
| year | flat-cp037 | 774000 | 24.26 | 30.43 | 5.29 |
| decade | canonical-json | 15818299 | 163.73 | 92.14 | 51.66 |
| decade | camt | 23155368 | 517.29 | 42.69 | 104.96 |
| decade | flat-cp037 | 7657600 | 192.20 | 38.00 | 52.25 |
| stress | canonical-json | 85105435 | 859.84 | 94.39 | 334.72 |
| stress | camt | 121440616 | 2390.56 | 48.45 | 490.58 |
| stress | flat-cp037 | 40000800 | 944.72 | 40.38 | 262.41 |

## Mutation experiment

12 matched seeds, 192 cases per strategy per seed, 13 isolated mutants.

| Strategy | Cases | Operation inputs | Kills / mutant runs | Score |
|---|---:|---:|---:|---:|
| naive | 2304 | 2304 | 37 / 156 | 23.72% |
| ssfd | 2304 | 2556 | 24 / 156 | 15.38% |
| ssfd-chaos | 2304 | 3835 | 142 / 156 | 91.03% |

| Mutant | Naive seeds | SSFD seeds | SSFD + chaos seeds |
|---|---:|---:|---:|
| fx_ties_round_up | 12 | 12 | 10 |
| missing_idempotency | 0 | 0 | 12 |
| leap_day_interest_omitted | 1 | 0 | 12 |
| dst_fold_collision | 0 | 0 | 12 |
| overdraft_floor_exclusive | 0 | 0 | 12 |
| refund_cap_missing | 0 | 0 | 12 |
| currency_mismatch_accepted | 12 | 0 | 0 |
| frozen_debit_bypass | 0 | 0 | 12 |
| clearing_without_authorization | 0 | 0 | 12 |
| reversal_credit_omitted | 0 | 0 | 12 |
| cutoff_rounded_to_second | 0 | 0 | 12 |
| settlement_credit_short_one | 12 | 12 | 12 |
| stale_authorization_balance_write | 0 | 0 | 12 |

| Paired comparison | First higher | Second higher | Ties | Mean difference (pp) | Exact two-sided sign p |
|---|---:|---:|---:|---:|---:|
| ssfd vs. naive | 0 | 12 | 0 | -8.33 | 0.000488 |
| ssfd-chaos vs. naive | 12 | 0 | 0 | +67.31 | 0.000488 |
| ssfd-chaos vs. ssfd | 12 | 0 | 0 | +75.64 | 0.000488 |

## One-million-journal generation and audit supplement


| Workload | Transactions | Journals | Postings | Generation ms (median [range]) | Journals/s | Postings/s | Peak heap MiB | Audit ms |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| stress | 1000000 | 1000000 | 2000000 | 20154.52 [18263.84–22045.20] | 50057 | 100114 | 4751.95 | 325.58 |

All reported journal and account audits passed. Repeated output digest comparison passed: **true**.

| Workload | Currency | Trial-balance debits (minor units) | Credits (minor units) | Difference (minor units) |
|---|---|---:|---:|---:|
| stress | GBP | 25002350514 | 25002350514 | 0 |
| stress | USD | 24981853198 | 24981853198 | 0 |

The two independently generated one-million-journal snapshots produced the same streamed canonical SHA-256, `5d4a6678a6ba767f104f1f376bbad410d038273ada2a80ddf63c07d94a9ec29b`. Peak measured generation heap was 4,751.95 MiB (4.64 GiB). Bulk serialization at one million journals was outside this measured profile. At 100,000 journals the measured CAMT stage had the largest sampled serializer heap at 490.58 MiB and encoded 121,440,616 bytes in a median 2.39 seconds.

## Interpretation and limitations

The aggressive-chaos strategy detected 142 of 156 mutant runs (91.03%) and scored higher than both controls on all 12 paired seeds. Its advantage over the naive control was 67.31 percentage points on average. The naive strategy detected the domestic currency mismatch on all 12 seeds because independent currency draws sometimes produce an invalid pair. Both structured strategies preserve currency coherence in their domestic transfers, so that mutant survived every structured run. The happy-path strategy therefore fell below the naive control on this particular mutant set. The aggressive corpus also missed two FX rounding cases after fixed-budget stratification. This result distinguishes balanced generation from the negative cases needed to test rejection behavior.

All three strategies used 2,304 cases apiece. They exercised 2,304, 2,556 and 3,835 operation inputs respectively because lifecycle scenarios contain multiple operations. Higher detection is measured at an equal case budget. A per-operation comparison or other sampling allocations could produce different results. Detection is specific to the two-account mock SUT, its chosen 13 mutants and the deterministic calendar-year fixture. Its stale-update mutation models a concrete interleaving rather than real thread scheduling. The paired exact sign probabilities describe direction consistency under the paired binomial assumption for this experiment. The fixed seeds and mutually related mutants limit generalization.

Every committed journal in the measured workloads balanced independently by currency. The reconstructed account balances agreed with the snapshot and all GBP and USD trial-balance differences were zero. The independent audit detects per-entry imbalance even if another entry would offset it. This establishes conservation for the measured histories. It cannot establish realism or fit for a particular bank.

Performance figures are descriptive for one environment and small repetition counts. The stress workload exercises pure fully committed deposit lifecycles in one book, so differences between it and the behavioral cases include workload shape. The largest sampled heap is a lower bound on instantaneous peak and is not whole-process resident memory. Native or multi-machine performance measurements, external core banking mutation targets, and workload calibration against operational distributions remain outside this evaluation.

## Compatibility checks

The race-enabled repository suite and `go vet ./...` passed. The prior Phase 2 seed-42 fixture, the Phase 3 seed-42 snapshot and the Phase 4 aggressive seed-42 snapshot kept their recorded SHA-256 values. The Phase 5 packages and archived exports are byte-for-byte identical to their inputs. The archived `pain.001.001.09` and `camt.053.001.08` both passed the pinned official XSD validator again.
