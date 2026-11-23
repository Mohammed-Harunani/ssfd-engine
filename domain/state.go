package domain

import "time"

type AccountState uint8

const (
	Active AccountState = iota + 1
	Dormant
	Frozen
	Closed
)

type AccountEvent struct {
	Book       BookID
	Account    AccountID
	Sequence   uint64
	RecordedAt time.Time
	From       AccountState
	To         AccountState
}

// CanTransitionTo checks the lifecycle topology. Authorization, account
// balances, and outstanding obligations are evaluated alongside this check.
func (s AccountState) CanTransitionTo(next AccountState) bool {
	switch s {
	case Active:
		return next == Dormant || next == Frozen || next == Closed
	case Dormant:
		return next == Active || next == Frozen || next == Closed
	case Frozen:
		return next == Active || next == Dormant
	default:
		return false
	}
}

type TransactionState uint8

const (
	Pending TransactionState = iota + 1
	Authorized
	Cleared
	Reversed
	Disputed
	Cancelled
	Declined
)

type TransactionEvent struct {
	Book        BookID
	Transaction TransactionID
	Sequence    uint64
	RecordedAt  time.Time
	From        TransactionState
	To          TransactionState
	Journal     EntryID
}

// CanTransitionTo checks the workflow topology. Journal and authorization
// dependencies are checked against the corresponding committed facts.
func (s TransactionState) CanTransitionTo(next TransactionState) bool {
	switch s {
	case Pending:
		return next == Authorized || next == Declined || next == Cancelled
	case Authorized:
		return next == Cleared || next == Cancelled
	case Cleared:
		return next == Disputed || next == Reversed
	case Disputed:
		return next == Cleared || next == Reversed
	default:
		return false
	}
}
