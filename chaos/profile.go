// Package chaos injects deterministic banking scenarios into a synthetic book.
package chaos

import "fmt"

const Version = "ssfd-chaos-v1"

type Fault string

const (
	Ordering   Fault = "early_clearing_and_delay"
	Duplicate  Fault = "idempotency_replay"
	Midnight   Fault = "midnight_cutoff"
	LeapDay    Fault = "leap_day_accrual"
	DSTOverlap Fault = "dst_overlap"
	Overdraft  Fault = "overdraft_boundary"
	Chargeback Fault = "partial_refund_and_chargeback"
	Freeze     Fault = "aml_freeze_and_reversal"
)

var order = [...]Fault{Midnight, LeapDay, DSTOverlap, Ordering, Duplicate, Overdraft, Chargeback, Freeze}

// Config gives each fault an independent probability in basis points.
// Unspecified weights disable that fault. Calendar faults draw only on
// applicable local dates. A run copies the weights.
type Config struct {
	Seed    uint64
	Weights map[Fault]uint16
}

func Profile(name string, seed uint64) (Config, error) {
	c := Config{Seed: seed, Weights: make(map[Fault]uint16)}
	switch name {
	case "standard":
		c.Weights = map[Fault]uint16{Ordering: 450, Duplicate: 350, Midnight: 1000, LeapDay: 10000,
			DSTOverlap: 10000, Overdraft: 400, Chargeback: 450, Freeze: 250}
	case "aggressive":
		for _, fault := range order {
			c.Weights[fault] = 10000
		}
	default:
		return Config{}, fmt.Errorf("unknown chaos profile %q", name)
	}
	return c, nil
}

type FaultStats struct {
	Fault              Fault
	WeightBPS          uint16
	Eligible           uint64
	Injected           uint64
	Rejected           uint64
	IdenticalRetries   uint64
	Journals           uint64
	DelayedSettlements uint64
}
