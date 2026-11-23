package agents

import (
	"fmt"
	"strconv"

	"ssfd-engine/domain"
	"ssfd-engine/ledger"
)

type payment struct {
	activity         Activity
	debtor, creditor domain.CounterpartyID
	funding          domain.AccountID
	principal        domain.Money
	quote            *domain.FXQuote
	postings         []domain.Posting
	installment      *Installment
	position         uint32
}

func leg(account domain.AccountID, side domain.Side, amount domain.Money) domain.Posting {
	return domain.Posting{Account: account, Side: side, Amount: amount}
}

func gbp(minor int64) domain.Money { return domain.Money{Currency: "GBP", Minor: minor} }

func (e *engine) available(account domain.AccountID) (int64, error) {
	view, ok := e.book.Account(account)
	if !ok {
		return 0, fmt.Errorf("unknown funding account %d", account)
	}
	n, err := strconv.ParseInt(view.AvailableNormal, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("model account %d outside configured amount range: %w", account, err)
	}
	return n, nil
}

func (e *engine) execute(p payment) (bool, error) {
	e.transaction++
	id := e.transaction
	kind := domain.SingleCurrency
	if p.quote != nil {
		kind = domain.Exchange
	}
	_, err := e.apply(ledger.Command{OpenTransaction: &ledger.OpenTransaction{ID: id, Kind: kind, Debtor: p.debtor,
		Creditor: p.creditor, Principal: p.principal, Quote: p.quote}})
	if err != nil {
		return false, err
	}
	event := Event{Activity: p.activity, At: e.clock.Now(), Transaction: id, Debtor: p.debtor, Creditor: p.creditor,
		Principal: p.principal, Quote: p.quote, LoanPosition: p.position, LoanPayment: p.installment}
	if p.funding != 0 {
		n, err := e.available(p.funding)
		if err != nil {
			return false, err
		}
		if n < p.principal.Minor {
			_, err := e.apply(ledger.Command{ChangeTransaction: &ledger.ChangeTransaction{ID: id,
				From: domain.Pending, To: domain.Declined}})
			if err != nil {
				return false, err
			}
			event.Outcome = domain.Declined
			e.events = append(e.events, event)
			return false, nil
		}
	}
	_, err = e.apply(ledger.Command{ChangeTransaction: &ledger.ChangeTransaction{ID: id, From: domain.Pending,
		To: domain.Authorized, ReserveAccount: p.funding}})
	if err != nil {
		return false, err
	}
	e.entry++
	receipt, err := e.apply(ledger.Command{ChangeTransaction: &ledger.ChangeTransaction{ID: id,
		From: domain.Authorized, To: domain.Cleared, Journal: &ledger.Journal{ID: e.entry, Kind: domain.Booking,
			EffectiveAt: e.clock.Now(), Postings: p.postings}}})
	if err != nil {
		return false, err
	}
	event.Entry, event.Outcome = receipt.Entry, domain.Cleared
	e.events = append(e.events, event)
	return true, nil
}

func (e *engine) transfer(kind Activity, from, to Actor, source, recipient ActorAccount, minor int64) (bool, error) {
	m := gbp(minor)
	return e.execute(payment{activity: kind, debtor: from.ID, creditor: to.ID, funding: source.ID, principal: m,
		postings: []domain.Posting{leg(source.ID, domain.Debit, m), leg(recipient.ID, domain.Credit, m)}})
}

func (e *engine) internal(kind Activity, debit, credit domain.AccountID, recipient domain.CounterpartyID, minor int64) error {
	m := gbp(minor)
	_, err := e.execute(payment{activity: kind, debtor: e.p.bank, creditor: recipient, principal: m,
		postings: []domain.Posting{leg(debit, domain.Debit, m), leg(credit, domain.Credit, m)}})
	return err
}

func (e *engine) exchange(from, to Actor, minor int64) error {
	m := gbp(minor)
	quote, err := e.rate.Convert(currencies(), m, e.clock.Now())
	if err != nil {
		return err
	}
	_, err = e.execute(payment{activity: CrossBorder, debtor: from.ID, creditor: to.ID, funding: from.Accounts[0].ID,
		principal: m, quote: &domain.FXQuote{Rate: e.rate.ID, Target: quote}, postings: []domain.Posting{
			leg(from.Accounts[0].ID, domain.Debit, m), leg(e.p.fxGBP, domain.Credit, m),
			leg(e.p.fxUSD, domain.Debit, quote), leg(to.Accounts[2].ID, domain.Credit, quote),
		}})
	return err
}

func (e *engine) repay(l *loan, day date) error {
	if l.outstanding == 0 {
		return nil
	}
	position := l.paid + 1
	i, err := l.terms.Payment(l.outstanding, position, l.last, day)
	if err != nil {
		return err
	}
	total := gbp(i.Total)
	funding := l.borrower.Accounts[0].ID
	settled, err := e.execute(payment{activity: l.kind, debtor: l.borrower.ID, creditor: e.p.bank, funding: funding,
		principal: total, installment: &i, position: position, postings: []domain.Posting{
			leg(funding, domain.Debit, total), leg(e.p.loanCollection, domain.Credit, total),
		}})
	if err != nil {
		return err
	}
	if !settled {
		return fmt.Errorf("scheduled loan payment declined for borrower %d at installment %d", l.borrower.ID, position)
	}
	postings := []domain.Posting{leg(e.p.loanCollection, domain.Debit, total), leg(l.asset, domain.Credit, gbp(i.Principal))}
	if i.Interest != 0 {
		postings = append(postings, leg(e.p.interest, domain.Credit, gbp(i.Interest)))
	}
	_, err = e.execute(payment{activity: LoanAllocation, debtor: e.p.bank, creditor: e.p.bank, principal: total,
		installment: &i, position: position, postings: postings})
	if err != nil {
		return err
	}
	l.outstanding -= i.Principal
	l.paid, l.last = position, day
	return nil
}
