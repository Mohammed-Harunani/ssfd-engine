// Command ssfd-eval runs performance and defect-detection experiments.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"strings"

	"ssfd-engine/eval"
)

func main() {
	log.SetFlags(0)
	mode := flag.String("mode", "all", "all, benchmark, or mutation")
	format := flag.String("format", "json", "json or markdown")
	workload := flag.String("workload", "all", "all, year, decade, or stress")
	seed := flag.Uint64("seed", 42, "benchmark seed and first paired mutation seed")
	repeats := flag.Int("repeats", 3, "performance repetitions per workload")
	stress := flag.Int("stress-journals", 100000, "journal count for the stress book")
	only := flag.Bool("generation-only", false, "skip bulk serialization; verify streamed determinism")
	seedCount := flag.Int("mutation-seeds", 12, "number of consecutive paired experiment seeds")
	budget := flag.Int("cases", 192, "number of test cases per strategy per seed")
	mutant := flag.String("mutant", "", "optional comma-separated mutation identifiers")
	flag.Parse()
	if flag.NArg() != 0 {
		log.Fatal("unexpected positional arguments")
	}
	if *format != "json" && *format != "markdown" {
		log.Fatal("unknown report format")
	}
	if *mode != "all" && *mode != "benchmark" && *mode != "mutation" {
		log.Fatal("unknown evaluation mode")
	}
	var report eval.ResearchReport
	if *mode == "all" || *mode == "benchmark" {
		workloads := []eval.Workload{eval.Year, eval.Decade, eval.Stress}
		if *workload != "all" {
			workloads = []eval.Workload{eval.Workload(*workload)}
		}
		r, err := eval.Benchmarks(eval.BenchmarkConfig{Workloads: workloads, Seed: *seed, StressJournals: *stress, Repeats: *repeats, GenerationOnly: *only})
		if err != nil {
			log.Fatal(err)
		}
		report.Benchmark = &r
	}
	if *mode == "all" || *mode == "mutation" {
		if *seedCount < 1 || *seedCount > 1000 || uint64(*seedCount-1) > math.MaxUint64-*seed {
			log.Fatal("invalid mutation seed span")
		}
		seeds := make([]uint64, *seedCount)
		for i := range seeds {
			seeds[i] = *seed + uint64(i)
		}
		selected := eval.Mutations()
		if *mutant != "" {
			selected = nil
			for _, s := range strings.Split(*mutant, ",") {
				selected = append(selected, eval.Mutation(strings.TrimSpace(s)))
			}
		}
		r, err := eval.EvaluateSelected(seeds, *budget, selected)
		if err != nil {
			log.Fatal(err)
		}
		report.Mutation = &r
	}
	if *format == "markdown" {
		if _, err := fmt.Fprint(os.Stdout, report.Markdown()); err != nil {
			log.Fatal(err)
		}
	} else {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			log.Fatal(err)
		}
	}
}
