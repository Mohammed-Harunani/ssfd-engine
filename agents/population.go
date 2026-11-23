package agents

import (
	"fmt"

	"ssfd-engine/domain"
	"ssfd-engine/ledger"
)

type Archetype string

const (
	SalariedRetail       Archetype = "salaried_retail"
	SmallBusiness        Archetype = "small_business"
	Landlord             Archetype = "landlord"
	UtilityProvider      Archetype = "utility_provider"
	SubscriptionProvider Archetype = "subscription_provider"
)

type AccountRole string

const (
	Current           AccountRole = "current"
	Savings           AccountRole = "savings"
	Operating         AccountRole = "operating"
	Reserve           AccountRole = "reserve"
	RentalReceipts    AccountRole = "rental_receipts"
	PropertyReserve   AccountRole = "property_reserve"
	Collection        AccountRole = "collection"
	Treasury          AccountRole = "treasury"
	ForeignSettlement AccountRole = "foreign_settlement"
)

type ActorAccount struct {
	ID       domain.AccountID
	Role     AccountRole
	Currency domain.Currency
}

type Actor struct {
	ID        domain.CounterpartyID
	Archetype Archetype
	Domicile  string
	Accounts  []ActorAccount
}

type population struct {
	bank                                         domain.CounterpartyID
	actors                                       []Actor
	parties                                      []domain.Counterparty
	accounts                                     []ledger.OpenAccount
	retail, business                             []Actor
	landlord, utility, subscription              Actor
	cash, fxGBP, fxUSD, loanCollection, interest domain.AccountID
	loans                                        []loan
}

func buildPopulation(startDate date) population {
	p := population{bank: 1, parties: []domain.Counterparty{{ID: 1, Kind: domain.Institution}}}
	p.cash = p.addAccount(0, domain.Asset, domain.General, "GBP", false)
	p.fxGBP = p.addAccount(0, domain.Asset, domain.FXPosition, "GBP", false)
	p.fxUSD = p.addAccount(0, domain.Asset, domain.FXPosition, "USD", false)
	p.loanCollection = p.addAccount(p.bank, domain.Liability, domain.General, "GBP", true)
	p.interest = p.addAccount(0, domain.Revenue, domain.General, "GBP", false)
	for range 6 {
		p.retail = append(p.retail, p.addActor(SalariedRetail, domain.Individual, Current, Savings))
	}
	for range 3 {
		p.business = append(p.business, p.addActor(SmallBusiness, domain.Business, Operating, Reserve))
	}
	// The third business is a US merchant that settles cross-border sales in USD.
	i := len(p.actors) - 1
	foreign := p.addAccount(p.actors[i].ID, domain.Liability, domain.General, "USD", true)
	p.actors[i].Domicile = "US"
	p.actors[i].Accounts = append(p.actors[i].Accounts, ActorAccount{foreign, ForeignSettlement, "USD"})
	p.business[2] = p.actors[i]
	p.landlord = p.addActor(Landlord, domain.Individual, RentalReceipts, PropertyReserve)
	p.utility = p.addActor(UtilityProvider, domain.Business, Collection, Treasury)
	p.subscription = p.addActor(SubscriptionProvider, domain.Business, Collection, Treasury)
	p.loans = []loan{
		{borrower: p.retail[4], asset: p.addAccount(0, domain.Asset, domain.General, "GBP", true),
			terms: LoanTerms{Principal: 24_000_000, AnnualBasisPoints: 395, Installments: 240, Convention: Actual365Fixed},
			last:  startDate, kind: Mortgage},
		{borrower: p.business[1], asset: p.addAccount(0, domain.Asset, domain.General, "GBP", true),
			terms: LoanTerms{Principal: 600_000, AnnualBasisPoints: 820, Installments: 18, Convention: Thirty360ISDA},
			last:  startDate, kind: BusinessLoan},
	}
	for i := range p.loans {
		p.loans[i].outstanding = p.loans[i].terms.Principal
	}
	return p
}

func (p *population) addAccount(owner domain.CounterpartyID, class domain.AccountClass, purpose domain.AccountPurpose, currency domain.Currency, floor bool) domain.AccountID {
	id := domain.AccountID(len(p.accounts) + 1)
	p.accounts = append(p.accounts, ledger.OpenAccount{ID: id, Owner: owner, Class: class, Purpose: purpose, Currency: currency,
		Policy: ledger.AccountPolicy{EnforceFloor: floor}})
	return id
}

func (p *population) addActor(kind Archetype, party domain.PartyKind, roles ...AccountRole) Actor {
	id := domain.CounterpartyID(len(p.parties) + 1)
	p.parties = append(p.parties, domain.Counterparty{ID: id, Kind: party})
	a := Actor{ID: id, Archetype: kind, Domicile: "GB"}
	for _, role := range roles {
		account := p.addAccount(id, domain.Liability, domain.General, "GBP", true)
		a.Accounts = append(a.Accounts, ActorAccount{account, role, "GBP"})
	}
	p.actors = append(p.actors, a)
	return a
}

func (p population) verify() error {
	for _, a := range p.actors {
		if len(a.Accounts) < 2 || a.Accounts[0].Currency != "GBP" || a.Accounts[1].Currency != "GBP" {
			return fmt.Errorf("actor %d missing standard account topology", a.ID)
		}
	}
	return nil
}
