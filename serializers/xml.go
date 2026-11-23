package serializers

import (
	"encoding/xml"
	"fmt"
	"math/big"
	"strings"
	"time"

	"ssfd-engine/domain"
	"ssfd-engine/ledger"
)

const (
	PainNamespace = "urn:iso:std:iso:20022:tech:xsd:pain.001.001.09"
	CamtNamespace = "urn:iso:std:iso:20022:tech:xsd:camt.053.001.08"
)

type xmlAmount struct {
	Currency domain.Currency `xml:"Ccy,attr"`
	Value    string          `xml:",chardata"`
}

func amountXML(c Catalog, code domain.Currency, n *big.Int) (xmlAmount, error) {
	spec, err := c.lookup(code)
	if err != nil {
		return xmlAmount{}, err
	}
	if n.Sign() < 0 {
		return xmlAmount{}, fmt.Errorf("negative XML magnitude")
	}
	s := decimal(n, spec.Digits)
	parts := strings.SplitN(s, ".", 2)
	integer := strings.TrimLeft(parts[0], "0")
	fraction := ""
	if len(parts) == 2 {
		fraction = strings.TrimRight(parts[1], "0")
	}
	if len(fraction) > 5 || len(integer)+len(fraction) > 18 {
		return xmlAmount{}, fmt.Errorf("amount %s %s exceeds ISO 20022 amount precision", s, code)
	}
	return xmlAmount{code, s}, nil
}

type xmlAccount struct {
	ID       string          `xml:"Id>Othr>Id"`
	Currency domain.Currency `xml:"Ccy"`
}

func existingAccount(a ledger.AccountView) xmlAccount {
	return xmlAccount{accountID(a.Account.ID), a.Account.Currency}
}

type messageHeader struct {
	ID      string `xml:"MsgId"`
	Created string `xml:"CreDtTm"`
}

func instant(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func (m model) created() string {
	var latest time.Time
	consider := func(t time.Time) {
		if t.After(latest) {
			latest = t
		}
	}
	for _, a := range m.snapshot.Accounts {
		consider(a.Account.OpenedAt)
	}
	for _, e := range m.snapshot.Journals {
		consider(e.RecordedAt)
	}
	for _, e := range m.snapshot.AccountEvents {
		consider(e.RecordedAt)
	}
	for _, e := range m.snapshot.TransactionEvents {
		consider(e.RecordedAt)
	}
	for _, e := range m.snapshot.HoldEvents {
		consider(e.RecordedAt)
	}
	return instant(latest)
}

func document(v any) ([]byte, error) {
	body, err := xml.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(append([]byte(xml.Header), body...), '\n'), nil
}

type painDocument struct {
	XMLName    xml.Name       `xml:"Document"`
	Namespace  string         `xml:"xmlns,attr"`
	Initiation painInitiation `xml:"CstmrCdtTrfInitn"`
}

type painInitiation struct {
	Header   painHeader    `xml:"GrpHdr"`
	Payments []painPayment `xml:"PmtInf"`
}

type painHeader struct {
	messageHeader
	Count     int    `xml:"NbOfTxs"`
	Initiator string `xml:"InitgPty>Nm"`
}

type equivalent struct {
	Amount xmlAmount       `xml:"Amt"`
	Target domain.Currency `xml:"CcyOfTrf"`
}

const ProjectionNamespace = "urn:ssfd:projection:1"

type quoteProjection struct {
	Namespace     string        `xml:"xmlns,attr"`
	Rate          domain.RateID `xml:"RateID"`
	SettledTarget xmlAmount     `xml:"SettledTarget"`
}

type painCredit struct {
	Instruction     string           `xml:"PmtId>InstrId"`
	EndToEnd        string           `xml:"PmtId>EndToEndId"`
	Instructed      *xmlAmount       `xml:"Amt>InstdAmt,omitempty"`
	Equivalent      *equivalent      `xml:"Amt>EqvtAmt,omitempty"`
	Creditor        string           `xml:"Cdtr>Nm"`
	CreditorAccount xmlAccount       `xml:"CdtrAcct"`
	Quote           *quoteProjection `xml:"SplmtryData>Envlp>Quote,omitempty"`
}

type painPayment struct {
	ID            string     `xml:"PmtInfId"`
	Method        string     `xml:"PmtMtd"`
	Execution     string     `xml:"ReqdExctnDt>DtTm"`
	Debtor        string     `xml:"Dbtr>Nm"`
	DebtorAccount xmlAccount `xml:"DbtrAcct"`
	Bank          string     `xml:"DbtrAgt>FinInstnId>Nm"`
	Credit        painCredit `xml:"CdtTrfTxInf"`
}

// EncodePain projects the selected committed credit-transfer bookings in
// ledger order. Non-transfer activity must be omitted by the caller.
func EncodePain(s ledger.Snapshot, c Catalog, ids []domain.TransactionID) ([]byte, error) {
	m, err := prepare(s, c)
	if err != nil {
		return nil, err
	}
	return m.pain(c, ids)
}

func (m model) pain(c Catalog, ids []domain.TransactionID) ([]byte, error) {
	selected := make(map[domain.TransactionID]bool, len(ids))
	for _, id := range ids {
		if id == 0 || selected[id] {
			return nil, fmt.Errorf("duplicate or invalid payment selection %d", id)
		}
		selected[id] = true
	}
	out := painDocument{Namespace: PainNamespace}
	for _, e := range m.snapshot.Journals {
		if e.Kind != domain.Booking || !selected[e.Transaction] {
			continue
		}
		p, err := m.payment(c, e)
		if err != nil {
			return nil, err
		}
		out.Initiation.Payments = append(out.Initiation.Payments, p)
		delete(selected, e.Transaction)
	}
	if len(out.Initiation.Payments) == 0 || len(selected) != 0 {
		return nil, fmt.Errorf("pain requires booked credit transfer selections")
	}
	out.Initiation.Header = painHeader{messageHeader{ref("PAIN-B", uint64(m.snapshot.Book)), m.created()},
		len(out.Initiation.Payments), "SSFD Synthetic Initiator"}
	return document(out)
}

func (m model) payment(c Catalog, e domain.JournalEntry) (painPayment, error) {
	t := m.transactions[e.Transaction]
	var source, target ledger.AccountView
	destination := t.Principal
	if t.Quote != nil {
		destination = t.Quote.Target
	}
	for _, p := range e.Postings {
		a := m.accounts[p.Account]
		switch {
		case a.Account.Class == domain.Liability && a.Account.Owner == t.Debtor && p.Side == domain.Debit && p.Amount == t.Principal:
			if source.Account.ID != 0 {
				return painPayment{}, fmt.Errorf("multiple source legs in %d", e.ID)
			}
			source = a
		case a.Account.Class == domain.Liability && a.Account.Owner == t.Creditor && p.Side == domain.Credit && p.Amount == destination:
			if target.Account.ID != 0 {
				return painPayment{}, fmt.Errorf("multiple beneficiary legs in %d", e.ID)
			}
			target = a
		default:
			if t.Quote == nil || a.Account.Purpose != domain.FXPosition {
				return painPayment{}, fmt.Errorf("journal %d includes a non-credit-transfer leg", e.ID)
			}
		}
	}
	if source.Account.ID == 0 || target.Account.ID == 0 || t.Debtor == 0 || t.Creditor == 0 {
		return painPayment{}, fmt.Errorf("journal %d cannot project credit-transfer accounts", e.ID)
	}
	amt, err := amountXML(c, t.Principal.Currency, big.NewInt(t.Principal.Minor))
	if err != nil {
		return painPayment{}, err
	}
	credit := painCredit{Instruction: ref("J", uint64(e.ID)), EndToEnd: ref("T", uint64(t.ID)),
		Creditor: partyID(t.Creditor), CreditorAccount: existingAccount(target)}
	if t.Quote == nil {
		credit.Instructed = &amt
	} else {
		credit.Equivalent = &equivalent{amt, destination.Currency}
		targetAmount, err := amountXML(c, destination.Currency, big.NewInt(destination.Minor))
		if err != nil {
			return painPayment{}, err
		}
		credit.Quote = &quoteProjection{ProjectionNamespace, t.Quote.Rate, targetAmount}
	}
	return painPayment{ID: ref("P", uint64(e.ID)), Method: "TRF", Execution: instant(e.EffectiveAt),
		Debtor: partyID(t.Debtor), DebtorAccount: existingAccount(source), Bank: "SSFD Synthetic Bank", Credit: credit}, nil
}

type camtDocument struct {
	XMLName   xml.Name `xml:"Document"`
	Namespace string   `xml:"xmlns,attr"`
	Statement camtBody `xml:"BkToCstmrStmt"`
}

type camtBody struct {
	Header     messageHeader `xml:"GrpHdr"`
	Statements []statement   `xml:"Stmt"`
}

type statement struct {
	ID       string             `xml:"Id"`
	Account  xmlAccount         `xml:"Acct"`
	Balances []statementBalance `xml:"Bal"`
	Entries  []statementEntry   `xml:"Ntry"`
}

type statementBalance struct {
	Kind      string    `xml:"Tp>CdOrPrtry>Cd"`
	Amount    xmlAmount `xml:"Amt"`
	Direction string    `xml:"CdtDbtInd"`
	Date      string    `xml:"Dt>DtTm"`
}

type statementEntry struct {
	Reference string    `xml:"NtryRef"`
	Amount    xmlAmount `xml:"Amt"`
	Direction string    `xml:"CdtDbtInd"`
	Reversal  bool      `xml:"RvslInd,omitempty"`
	Status    string    `xml:"Sts>Cd"`
	Booked    string    `xml:"BookgDt>DtTm"`
	ValueDate string    `xml:"ValDt>DtTm"`
	Journal   string    `xml:"AcctSvcrRef"`
	Code      string    `xml:"BkTxCd>Prtry>Cd"`
	Source    string    `xml:"BkTxCd>Prtry>Issr"`
	Canonical string    `xml:"AddtlNtryInf"`
}

// EncodeCamt emits an inception-to-snapshot statement for every account.
func EncodeCamt(s ledger.Snapshot, c Catalog) ([]byte, error) {
	m, err := prepare(s, c)
	if err != nil {
		return nil, err
	}
	return m.camt(c)
}

func (m model) camt(c Catalog) ([]byte, error) {
	if len(m.snapshot.Accounts) == 0 {
		return nil, fmt.Errorf("camt requires at least one account")
	}
	created := m.created()
	out := camtDocument{Namespace: CamtNamespace, Statement: camtBody{Header: messageHeader{ref("CAMT-B", uint64(m.snapshot.Book)), created}}}
	for _, v := range m.snapshot.Accounts {
		code := v.Account.Currency
		debitNormal := v.Account.Class == domain.Asset || v.Account.Class == domain.Expense
		n, _ := new(big.Int).SetString(v.DebitPositive, 10) // prepare has checked the exact canonical string.
		if !debitNormal {
			n.Neg(n)
		}
		direction := "CRDT"
		if n.Sign() < 0 {
			direction = "DBIT"
		}
		act, err := amountXML(c, code, n.Abs(n))
		if err != nil {
			return nil, err
		}
		zero, err := amountXML(c, code, new(big.Int))
		if err != nil {
			return nil, err
		}
		a := statement{ID: accountID(v.Account.ID), Account: existingAccount(v), Balances: []statementBalance{
			{"OPBD", zero, "CRDT", instant(v.Account.OpenedAt)}, {"CLBD", act, direction, created}}}
		for _, j := range m.byAccount[v.Account.ID] {
			e, p := j.Entry, j.Posting
			amount, err := amountXML(c, code, big.NewInt(p.Amount.Minor))
			if err != nil {
				return nil, err
			}
			dir := "DBIT"
			if (p.Side == domain.Debit) == debitNormal {
				dir = "CRDT"
			}
			a.Entries = append(a.Entries, statementEntry{
				Reference: fmt.Sprintf("J%d-P%d", e.ID, j.Position), Amount: amount, Direction: dir, Reversal: e.Kind == domain.Reversal,
				Status: "BOOK", Booked: instant(e.RecordedAt), ValueDate: instant(e.EffectiveAt), Journal: ref("J", uint64(e.ID)),
				Code: fmt.Sprintf("SSFD-%d", e.Kind), Source: "SSFD",
				Canonical: fmt.Sprintf("transaction=%d;sequence=%d;side=%d;original=%d", e.Transaction, e.CommitSequence, p.Side, e.Original)})
		}
		out.Statement.Statements = append(out.Statement.Statements, a)
	}
	return document(out)
}
