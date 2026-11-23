package ledger

import (
	"fmt"
	"math/big"
	"sort"

	"ssfd-engine/domain"
)

// PostingRef identifies a graph incidence. Credit edges point from account to
// journal and debit edges point from journal to account.
type PostingRef struct {
	Entry    domain.EntryID
	Position uint64
	Side     domain.Side
}

type AccountView struct {
	Account domain.Account
	// All quantities are canonical base-ten integer strings in account units.
	DebitPositive   string
	Reserved        string
	AvailableNormal string
	HoldCount       int
}

// Account returns a current account projection plus its exact balances.
func (l *Ledger) Account(id domain.AccountID) (AccountView, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.accountView(id)
}

func (l *Ledger) accountView(id domain.AccountID) (AccountView, bool) {
	a, ok := l.accounts[id]
	if !ok {
		return AccountView{}, false
	}
	d := l.accountDetails[id]
	available := normalBalance(a, &d.balance)
	available.Sub(available, &d.reserved)
	return AccountView{a, d.balance.String(), d.reserved.String(), available.String(), len(d.holds)}, true
}

// Edges returns account incidences in commit order, then posting order.
func (l *Ledger) Edges(id domain.AccountID) []PostingRef {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if a := l.accountDetails[id]; a != nil {
		return append([]PostingRef(nil), a.edges...)
	}
	return nil
}

type TrialRow struct {
	Currency domain.Currency
	// Totals use account net balances, grouped by debit and credit.
	Debits, Credits, Difference string
}

// Snapshot is a consistent, immutable-by-ownership copy of a committed prefix.
type Snapshot struct {
	Book              domain.BookID
	Sequence          uint64
	Accounts          []AccountView
	Transactions      []domain.Transaction
	Journals          []domain.JournalEntry
	AccountEvents     []domain.AccountEvent
	TransactionEvents []domain.TransactionEvent
	HoldEvents        []HoldEvent
}

func (l *Ledger) Snapshot() Snapshot {
	l.mu.RLock()
	defer l.mu.RUnlock()
	s := Snapshot{Book: l.book, Sequence: l.sequence}
	for _, id := range orderedIDs(l.accounts) {
		view, _ := l.accountView(id)
		s.Accounts = append(s.Accounts, view)
	}
	for _, id := range orderedIDs(l.transactions) {
		tx := l.transactions[id]
		tx.Quote = copyQuote(tx.Quote)
		s.Transactions = append(s.Transactions, tx)
	}
	for _, id := range l.journalOrder {
		entry := l.entries[id]
		entry.Postings = append([]domain.Posting(nil), entry.Postings...)
		s.Journals = append(s.Journals, entry)
	}
	s.AccountEvents = append([]domain.AccountEvent(nil), l.accountHistory...)
	s.TransactionEvents = append([]domain.TransactionEvent(nil), l.transactionHistory...)
	s.HoldEvents = append([]HoldEvent(nil), l.holdHistory...)
	return s
}

func orderedIDs[K ~uint64, V any](values map[K]V) []K {
	ids := make([]K, 0, len(values))
	for id := range values {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// Audit independently reconstructs account balances from journal postings,
// checks the incidence index, then produces a sorted per-currency trial balance.
func (l *Ledger) Audit() ([]TrialRow, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	replayed := make(map[domain.AccountID]*big.Int)
	nextEdge := make(map[domain.AccountID]int)
	var previousSequence uint64
	for _, id := range l.journalOrder {
		e := l.entries[id]
		if e.CommitSequence <= previousSequence || e.CommitSequence > l.sequence {
			return nil, fmt.Errorf("invalid journal commit order at %d", id)
		}
		previousSequence = e.CommitSequence
		if err := e.Validate(l.catalog, l.accounts); err != nil {
			return nil, fmt.Errorf("audit journal %d: %w", id, err)
		}
		for i, p := range e.Postings {
			if replayed[p.Account] == nil {
				replayed[p.Account] = new(big.Int)
			}
			amount := big.NewInt(p.Amount.Minor)
			if p.Side == domain.Credit {
				amount.Neg(amount)
			}
			replayed[p.Account].Add(replayed[p.Account], amount)
			idx := nextEdge[p.Account]
			edges := l.accountDetails[p.Account].edges
			if idx >= len(edges) || edges[idx] != (PostingRef{id, uint64(i) + 1, p.Side}) {
				return nil, fmt.Errorf("audit graph mismatch at journal %d posting %d", id, i+1)
			}
			nextEdge[p.Account]++
		}
	}
	type totals struct{ debit, credit big.Int }
	byCurrency := make(map[domain.Currency]*totals)
	for _, id := range orderedIDs(l.accounts) {
		a := l.accounts[id]
		d := l.accountDetails[id]
		b := replayed[id]
		if b == nil {
			b = new(big.Int)
		}
		if b.Cmp(&d.balance) != 0 || len(d.edges) != nextEdge[id] {
			return nil, fmt.Errorf("audit balance or graph mismatch at account %d", id)
		}
		total := byCurrency[a.Currency]
		if total == nil {
			total = new(totals)
			byCurrency[a.Currency] = total
		}
		if b.Sign() < 0 {
			total.credit.Sub(&total.credit, b)
		} else {
			total.debit.Add(&total.debit, b)
		}
	}
	currencies := make([]domain.Currency, 0, len(byCurrency))
	for code := range byCurrency {
		currencies = append(currencies, code)
	}
	sort.Slice(currencies, func(i, j int) bool { return currencies[i] < currencies[j] })
	result := make([]TrialRow, 0, len(currencies))
	for _, code := range currencies {
		t := byCurrency[code]
		difference := new(big.Int).Sub(&t.debit, &t.credit)
		if difference.Sign() != 0 {
			return nil, fmt.Errorf("audit discrepancy %s in %s", difference, code)
		}
		result = append(result, TrialRow{code, t.debit.String(), t.credit.String(), difference.String()})
	}
	return result, nil
}
