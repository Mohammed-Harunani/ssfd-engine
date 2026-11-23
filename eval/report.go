package eval

import (
	"fmt"
	"sort"
	"strings"
)

type ResearchReport struct {
	Benchmark *BenchmarkReport
	Mutation  *MutationReport
}

// Markdown renders descriptive measurements and paired mutation results.
func (r ResearchReport) Markdown() string {
	var out strings.Builder
	fmt.Fprintln(&out, "# SSFD-Engine measured evaluation")
	if b := r.Benchmark; b != nil {
		fmt.Fprintf(&out, "\n## Performance\n\n%s on %s/%s, GOMAXPROCS %d, %d logical CPUs visible, seed %d, %d repetitions.\n",
			b.GoVersion, b.OS, b.Arch, b.GOMAXPROCS, b.LogicalCPUs, b.Config.Seed, b.Config.Repeats)
		fmt.Fprintln(&out, "\n| Workload | Transactions | Journals | Postings | Generation ms (median [range]) | Journals/s | Postings/s | Peak heap MiB | Audit ms |")
		fmt.Fprintln(&out, "|---|---:|---:|---:|---:|---:|---:|---:|---:|")
		for _, w := range b.Config.Workloads {
			var rows []Measurement
			for _, v := range b.Runs {
				if v.Workload == w {
					rows = append(rows, v)
				}
			}
			if len(rows) == 0 {
				continue
			}
			f := rows[0]
			ms := values(rows, func(m Measurement) float64 { return millis(m.Generation) })
			fmt.Fprintf(&out, "| %s | %d | %d | %d | %.2f [%.2f–%.2f] | %.0f | %.0f | %.2f | %.2f |\n", w,
				f.Transactions, f.Journals, f.Postings, median(ms), ms[0], ms[len(ms)-1],
				median(values(rows, func(m Measurement) float64 { return m.JournalsPerSecond })),
				median(values(rows, func(m Measurement) float64 { return m.PostingsPerSecond })),
				maxHeap(rows, func(m Measurement) Stage { return m.Generation }),
				median(values(rows, func(m Measurement) float64 { return millis(m.Audit) })))
		}
		fmt.Fprintf(&out, "\nAll reported journal and account audits passed. Repeated output digest comparison passed: **%t**.\n", b.Deterministic)
		fmt.Fprintln(&out, "\n| Workload | Currency | Trial-balance debits (minor units) | Credits (minor units) | Difference (minor units) |")
		fmt.Fprintln(&out, "|---|---|---:|---:|---:|")
		for _, m := range b.Runs {
			if m.Repeat == 1 {
				for _, t := range m.TrialBalance {
					fmt.Fprintf(&out, "| %s | %s | %s | %s | %s |\n", m.Workload, t.Currency, t.Debits, t.Credits, t.Difference)
				}
			}
		}
		if !b.Config.GenerationOnly {
			fmt.Fprintln(&out, "\n| Workload | Encoder | Bytes | Median ms | Median MiB/s | Peak heap MiB |")
			fmt.Fprintln(&out, "|---|---|---:|---:|---:|---:|")
			for _, m := range b.Runs {
				if m.Repeat != 1 {
					continue
				}
				var rows []Measurement
				for _, other := range b.Runs {
					if other.Workload == m.Workload {
						rows = append(rows, other)
					}
				}
				for i, s := range m.Serialization {
					sel := func(v Measurement) Stage { return v.Serialization[i] }
					fmt.Fprintf(&out, "| %s | %s | %d | %.2f | %.2f | %.2f |\n", m.Workload, s.Name, s.OutputBytes,
						median(values(rows, func(v Measurement) float64 { return millis(sel(v)) })),
						median(values(rows, func(v Measurement) float64 {
							a := sel(v)
							return float64(a.OutputBytes) * 1e9 / (float64(a.Nanoseconds) * 1048576)
						})),
						maxHeap(rows, sel))
				}
			}
		}
	}
	if m := r.Mutation; m != nil {
		fmt.Fprintf(&out, "\n## Mutation experiment\n\n%d matched seeds, %d cases per strategy per seed, %d isolated mutants.\n", len(m.Runs), m.CasesPerSeed, len(m.Mutants))
		fmt.Fprintln(&out, "\n| Strategy | Cases | Operation inputs | Kills / mutant runs | Score |")
		fmt.Fprintln(&out, "|---|---:|---:|---:|---:|")
		for i, s := range m.Summary {
			var cases, ops int
			for _, v := range m.Runs {
				cases += v.Strategies[i].Cases
				ops += v.Strategies[i].Operations
			}
			fmt.Fprintf(&out, "| %s | %d | %d | %d / %d | %.2f%% |\n", s.Strategy, cases, ops, s.Kills, s.MutantRuns, s.ScorePercent)
		}
		fmt.Fprintln(&out, "\n| Mutant | Naive seeds | SSFD seeds | SSFD + chaos seeds |")
		fmt.Fprintln(&out, "|---|---:|---:|---:|")
		for _, name := range m.Mutants {
			fmt.Fprintf(&out, "| %s | %d | %d | %d |\n", name, m.Summary[0].KillsByMutation[name],
				m.Summary[1].KillsByMutation[name], m.Summary[2].KillsByMutation[name])
		}
		fmt.Fprintln(&out, "\n| Paired comparison | First higher | Second higher | Ties | Mean difference (pp) | Exact two-sided sign p |")
		fmt.Fprintln(&out, "|---|---:|---:|---:|---:|---:|")
		for _, p := range m.Paired {
			fmt.Fprintf(&out, "| %s vs. %s | %d | %d | %d | %+.2f | %.6f |\n", p.First, p.Second,
				p.FirstHigher, p.SecondHigher, p.Ties, p.MeanDifferencePP, p.TwoSidedExactP)
		}
	}
	return out.String()
}

func millis(s Stage) float64 { return float64(s.Nanoseconds) / 1e6 }

func values(rows []Measurement, f func(Measurement) float64) []float64 {
	out := make([]float64, len(rows))
	for i, row := range rows {
		out[i] = f(row)
	}
	sort.Float64s(out)
	return out
}

func median(s []float64) float64 { return (s[(len(s)-1)/2] + s[len(s)/2]) / 2 }

func maxHeap(rows []Measurement, sel func(Measurement) Stage) float64 {
	var peak uint64
	for _, r := range rows {
		peak = max(peak, sel(r).PeakHeapBytes)
	}
	return float64(peak) / 1048576
}
