// Package serializers projects immutable SSFD runs into versioned interchange profiles.
package serializers

import (
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"

	"ssfd-engine/domain"
	"ssfd-engine/ledger"
)

type Currency struct {
	Digits  uint8
	Numeric string
}

type Catalog map[domain.Currency]Currency

func SimulationCatalog() Catalog {
	return Catalog{"GBP": {2, "826"}, "USD": {2, "840"}}
}

func (c Catalog) lookup(code domain.Currency) (Currency, error) {
	s, ok := c[code]
	if !ok || len(code) != 3 || !digits(s.Numeric, 3) || s.Digits > 9 {
		return Currency{}, fmt.Errorf("unsupported currency %q", code)
	}
	for _, b := range []byte(code) {
		if b < 'A' || b > 'Z' {
			return Currency{}, fmt.Errorf("invalid currency %q", code)
		}
	}
	for other, v := range c {
		if other != code && v.Numeric == s.Numeric {
			return Currency{}, fmt.Errorf("ambiguous numeric currency %s", s.Numeric)
		}
	}
	return s, nil
}

// Decimal formats signed minor units as exact major units. It preserves scale.
func (c Catalog) Decimal(code domain.Currency, minor int64) (string, error) {
	spec, err := c.lookup(code)
	if err != nil {
		return "", err
	}
	return decimal(big.NewInt(minor), spec.Digits), nil
}

func decimal(n *big.Int, scale uint8) string {
	sign := ""
	if n.Sign() < 0 {
		sign = "-"
	}
	s := new(big.Int).Abs(n).String()
	if scale == 0 {
		return sign + s
	}
	m := int(scale)
	if len(s) <= m {
		s = strings.Repeat("0", m+1-len(s)) + s
	}
	return sign + s[:len(s)-m] + "." + s[len(s)-m:]
}

func digits(s string, size int) bool {
	if len(s) != size {
		return false
	}
	for _, b := range []byte(s) {
		if b < '0' || b > '9' {
			return false
		}
	}
	return true
}

type indexedPosting struct {
	Entry    domain.JournalEntry
	Position int
	Posting  domain.Posting
}

type model struct {
	snapshot     ledger.Snapshot
	accounts     map[domain.AccountID]ledger.AccountView
	transactions map[domain.TransactionID]domain.Transaction
	entries      map[domain.EntryID]domain.JournalEntry
	byAccount    map[domain.AccountID][]indexedPosting
	totals       []PostingTotals
	trial        []ledger.TrialRow
}

type PostingTotals struct {
	Currency                domain.Currency
	Count                   uint64
	DebitMinor, CreditMinor string
}

type total struct {
	count           uint64
	debits, credits big.Int
}

func sortedTotals(m map[domain.Currency]*total) []PostingTotals {
	out := make([]PostingTotals, 0, len(m))
	for code, t := range m {
		out = append(out, PostingTotals{code, t.count, t.debits.String(), t.credits.String()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Currency < out[j].Currency })
	return out
}

func accumulate(m map[domain.Currency]*total, c domain.Currency, n int64) {
	t := m[c]
	if t == nil {
		t = new(total)
		m[c] = t
	}
	t.count++
	if n > 0 {
		t.debits.Add(&t.debits, big.NewInt(n))
	} else {
		t.credits.Sub(&t.credits, big.NewInt(n))
	}
}

func prepare(s ledger.Snapshot, c Catalog) (model, error) {
	m := model{snapshot: s, accounts: make(map[domain.AccountID]ledger.AccountView),
		transactions: make(map[domain.TransactionID]domain.Transaction), entries: make(map[domain.EntryID]domain.JournalEntry),
		byAccount: make(map[domain.AccountID][]indexedPosting)}
	if s.Book == 0 {
		return m, fmt.Errorf("snapshot has no book")
	}
	dir := make(map[domain.AccountID]domain.Account)
	codes := make(domain.CurrencyCatalog)
	for _, v := range s.Accounts {
		a := v.Account
		if a.ID == 0 || a.Book != s.Book || dir[a.ID].ID != 0 {
			return m, fmt.Errorf("invalid or duplicate account %d", a.ID)
		}
		spec, err := c.lookup(a.Currency)
		if err != nil {
			return m, err
		}
		dir[a.ID], m.accounts[a.ID] = a, v
		codes[a.Currency] = domain.CurrencySpec{Code: a.Currency, MinorDigits: spec.Digits}
	}
	for _, t := range s.Transactions {
		if t.ID == 0 || t.Book != s.Book || m.transactions[t.ID].ID != 0 {
			return m, fmt.Errorf("invalid or duplicate transaction %d", t.ID)
		}
		m.transactions[t.ID] = t
	}
	totals := make(map[domain.Currency]*total)
	balances := make(map[domain.AccountID]*big.Int)
	var sequence uint64
	for _, e := range s.Journals {
		if e.Book != s.Book || sequence >= e.CommitSequence || e.CommitSequence > s.Sequence || m.entries[e.ID].ID != 0 ||
			m.transactions[e.Transaction].ID == 0 || (e.Original != 0 && m.entries[e.Original].Transaction != e.Transaction) {
			return m, fmt.Errorf("invalid journal linkage at %d", e.ID)
		}
		if err := e.Validate(codes, dir); err != nil {
			return m, fmt.Errorf("journal %d: %w", e.ID, err)
		}
		m.entries[e.ID], sequence = e, e.CommitSequence
		for i, p := range e.Postings {
			m.byAccount[p.Account] = append(m.byAccount[p.Account], indexedPosting{e, i + 1, p})
			n := p.Amount.Minor
			if p.Side == domain.Credit {
				n = -n
			}
			if balances[p.Account] == nil {
				balances[p.Account] = new(big.Int)
			}
			balances[p.Account].Add(balances[p.Account], big.NewInt(n))
			accumulate(totals, p.Amount.Currency, n)
		}
	}
	netTotals := make(map[domain.Currency]*total)
	for _, v := range s.Accounts {
		actual := "0"
		if b := balances[v.Account.ID]; b != nil {
			actual = b.String()
		}
		if actual != v.DebitPositive {
			return m, fmt.Errorf("account %d disagrees with replay", v.Account.ID)
		}
		n := netTotals[v.Account.Currency]
		if n == nil {
			n = new(total)
			netTotals[v.Account.Currency] = n
		}
		if b := balances[v.Account.ID]; b != nil {
			if b.Sign() < 0 {
				n.credits.Sub(&n.credits, b)
			} else {
				n.debits.Add(&n.debits, b)
			}
		}
	}
	for _, n := range sortedTotals(netTotals) {
		if n.DebitMinor != n.CreditMinor {
			return m, fmt.Errorf("unbalanced replay in %s", n.Currency)
		}
		m.trial = append(m.trial, ledger.TrialRow{Currency: n.Currency, Debits: n.DebitMinor, Credits: n.CreditMinor, Difference: "0"})
	}
	m.totals = sortedTotals(totals)
	return m, nil
}

func accountID(id domain.AccountID) string { return "A" + strconv.FormatUint(uint64(id), 10) }
func partyID(id domain.CounterpartyID) string {
	return "Synthetic Party " + strconv.FormatUint(uint64(id), 10)
}
func ref(prefix string, n uint64) string { return prefix + strconv.FormatUint(n, 10) }
