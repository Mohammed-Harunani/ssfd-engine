package eval

import (
	"testing"

	"ssfd-engine/domain"
)

func BenchmarkWorkloads(b *testing.B) {
	for _, w := range []Workload{Year, Decade, Stress} {
		b.Run(string(w), func(b *testing.B) {
			b.Run("generate", func(b *testing.B) {
				b.ReportAllocs()
				var n int
				for range b.N {
					f, err := generate(w, 42, 100000)
					if err != nil {
						b.Fatal(err)
					}
					n = len(f.Snapshot.Journals)
				}
				b.ReportMetric(float64(n)*float64(b.N)/b.Elapsed().Seconds(), "journals/s")
			})
			for _, codec := range []string{"canonical-json", "camt", "flat-cp037"} {
				b.Run(codec, func(b *testing.B) {
					f, err := generate(w, 42, 100000)
					if err != nil {
						b.Fatal(err)
					}
					b.ReportAllocs()
					b.ResetTimer()
					for range b.N {
						out, err := encode(codec, f.Snapshot)
						if err != nil {
							b.Fatal(err)
						}
						b.SetBytes(int64(len(out)))
					}
				})
			}
		})
	}
}

func TestStressDeterminismAndIndependentAudit(t *testing.T) {
	r, err := Benchmarks(BenchmarkConfig{Workloads: []Workload{Stress}, Seed: 0, StressJournals: 128, Repeats: 2})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Deterministic || len(r.Runs) != 2 {
		t.Fatalf("unexpected repeat report %+v", r)
	}
	for _, m := range r.Runs {
		if m.Transactions != 128 || m.Journals != 128 || m.Postings != 256 || len(m.TrialBalance) != 2 || len(m.Serialization) != 3 {
			t.Fatalf("unexpected stress record counts %+v", m)
		}
		for _, row := range m.TrialBalance {
			if row.Difference != "0" || row.Debits != row.Credits || row.Debits == "0" {
				t.Fatalf("bad trial balance %+v", row)
			}
		}
	}
	f, err := stressBook(42, 16)
	if err != nil {
		t.Fatal(err)
	}
	// Equal and opposite total errors still violate individual journal balance.
	f.Snapshot.Journals[0].Postings[0].Amount.Minor++
	f.Snapshot.Journals[2].Postings[1].Amount.Minor++
	if _, _, err := independentAudit(f.Snapshot); err == nil {
		t.Fatal("accepted equal and opposite journal imbalances")
	}
	f, err = stressBook(42, 4)
	if err != nil {
		t.Fatal(err)
	}
	f.Snapshot.Journals[0].Postings[0].Amount.Currency = domain.Currency("USD")
	if _, _, err := independentAudit(f.Snapshot); err == nil {
		t.Fatal("accepted currency mismatch")
	}
}
