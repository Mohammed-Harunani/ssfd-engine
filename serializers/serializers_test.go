package serializers

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"ssfd-engine/agents"
	"ssfd-engine/chaos"
	"ssfd-engine/domain"
)

func TestExactDecimal(t *testing.T) {
	c := SimulationCatalog()
	c["JPY"] = Currency{0, "392"}
	c["KWD"] = Currency{3, "414"}
	for _, x := range []struct {
		ccy  domain.Currency
		n    int64
		want string
	}{
		{"GBP", 7, "0.07"}, {"USD", 100, "1.00"}, {"JPY", 31, "31"}, {"KWD", 5, "0.005"},
		{"GBP", math.MinInt64, "-92233720368547758.08"},
	} {
		if got, err := c.Decimal(x.ccy, x.n); err != nil || got != x.want {
			t.Fatalf("decimal %v: %q %v", x, got, err)
		}
	}
	if _, err := c.Decimal("ZZZ", 1); err == nil {
		t.Fatal("unknown currency accepted")
	}
}

func TestCardBitmapAndRoundTrip(t *testing.T) {
	tm := time.Date(2024, 2, 29, 5, 6, 7, 0, time.UTC)
	const original = "020000012302290506070000000000100000000002"
	for _, tc := range []struct {
		typ            MTI
		mask, original string
	}{
		{Authorization, "3220000000008000", ""}, {Financial, "3220000000008000", ""},
		{CardReversal, "B2200000000080000000004000000000", original},
	} {
		msg := CardMessage{tc.typ, "000000", domain.Money{Currency: "GBP", Minor: 1234}, tm, 123, tc.original}
		for _, enc := range []BitmapEncoding{HexBitmap, BinaryBitmap} {
			raw, err := EncodeCard(msg, SimulationCatalog(), enc)
			if err != nil {
				t.Fatal(err)
			}
			wantMask := []byte(tc.mask)
			if enc == BinaryBitmap {
				wantMask, _ = hex.DecodeString(tc.mask)
			}
			if !bytes.Equal(raw[4:4+len(wantMask)], wantMask) {
				t.Fatalf("wrong bitmap for %s %s: %X", tc.typ, enc, raw)
			}
			wantFields := "0000000000000012340229050607000123826" + tc.original
			if string(raw[4+len(wantMask):]) != wantFields {
				t.Fatalf("incorrect fields: %q", raw)
			}
			got, err := DecodeCard(raw, SimulationCatalog(), enc, 2024)
			if err != nil || !reflect.DeepEqual(got, msg) {
				t.Fatalf("round-trip %+v: %v", got, err)
			}
			if _, err := DecodeCard(raw[:len(raw)-1], SimulationCatalog(), enc, 2024); err == nil {
				t.Fatal("accepted truncated message")
			}
			if _, err := DecodeCard(raw, SimulationCatalog(), enc, 2023); err == nil {
				t.Fatal("accepted leap day in 2023")
			}
		}
	}
}

func TestEBCDICZonedBytesAndFlatIntegrity(t *testing.T) {
	ts := time.Date(2024, 1, 1, 0, 0, 0, 123, time.UTC)
	b := FlatBatch{Book: 1, Details: []FlatDetail{
		{1, 1, 1, 0, 1, 1, "GBP", domain.Booking, 123, ts},
		{1, 1, 1, 0, 2, 2, "GBP", domain.Booking, -123, ts},
	}}
	for _, tc := range []struct {
		page                       CodePage
		bracket, open, plus, minus byte
	}{
		{ASCII, '[', 'H', 'C', 'L'},
		{CP037, 0xBA, 0xC8, 0xC3, 0xD3},
		{CP500, 0x4A, 0xC8, 0xC3, 0xD3},
	} {
		out, err := b.Marshal(tc.page)
		if err != nil {
			t.Fatal(err)
		}
		plus, minus := FlatRecordSize+amountOffset+18, 2*FlatRecordSize+amountOffset+18
		if len(out) != 5*FlatRecordSize || out[0] != tc.open || out[1] != tc.bracket ||
			out[plus] != tc.plus || out[minus] != tc.minus {
			t.Fatalf("bad fixed-width or code-page bytes for %s", tc.page)
		}
		if tc.page != ASCII && out[minus-1] != 0xF2 {
			t.Fatal("incorrect unsigned EBCDIC digit")
		}
		got, err := DecodeFlat(out, tc.page)
		if err != nil || !reflect.DeepEqual(got, b) {
			t.Fatalf("flat round-trip %s: %+v %v", tc.page, got, err)
		}
		out[len(out)-1] = 0
		if _, err := DecodeFlat(out, tc.page); err == nil {
			t.Fatal("corrupt trailer accepted")
		}
	}
	b.Details[0].DebitPositive++
	if _, err := b.Marshal(ASCII); err == nil {
		t.Fatal("unbalanced journal accepted")
	}
}

func TestBundleReconcilesToCanonical(t *testing.T) {
	r, err := agents.Run(agents.Config{Seed: 42, StartYear: 2024, Years: 1})
	if err != nil {
		t.Fatal(err)
	}
	c := SimulationCatalog()
	dir := filepath.Join(t.TempDir(), "exports")
	manifest, err := ExportRun(dir, r, nil, c)
	if err != nil {
		t.Fatal(err)
	}
	read := func(name string) []byte {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	for _, f := range manifest.Files {
		b := read(f.Name)
		if len(b) != f.Bytes || fmt.Sprintf("%x", sha256.Sum256(b)) != f.SHA256 {
			t.Fatalf("bad manifest entry %s", f.Name)
		}
	}
	var canonical struct{ Run agents.Result }
	if err := json.Unmarshal(read("canonical.json"), &canonical); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r, canonical.Run) {
		t.Fatal("canonical sidecar lost ledger or activity data")
	}
	entries := make(map[string]indexedPosting)
	totals := make(map[domain.Currency]*total)
	var details []FlatDetail
	for _, e := range r.Snapshot.Journals {
		for k, p := range e.Postings {
			entries[fmt.Sprintf("J%d-P%d", e.ID, k+1)] = indexedPosting{e, k + 1, p}
			n := p.Amount.Minor
			if p.Side == domain.Credit {
				n = -n
			}
			accumulate(totals, p.Amount.Currency, n)
			details = append(details, FlatDetail{e.ID, e.CommitSequence, e.Transaction, e.Original, p.Account, uint64(k + 1), p.Amount.Currency, e.Kind, n, e.EffectiveAt})
		}
	}
	if !slices.Equal(sortedTotals(totals), manifest.PostingTotals) || !slices.Equal(manifest.TrialBalance, r.TrialBalance) {
		t.Fatal("manifest did not reconcile")
	}
	for _, p := range []CodePage{ASCII, CP037, CP500} {
		b, err := DecodeFlat(read("ledger-"+string(p)+".dat"), p)
		if err != nil || b.Book != r.Snapshot.Book || !reflect.DeepEqual(b.Details, details) {
			t.Fatalf("flat projections shifted money or identity: %s %v", p, err)
		}
	}
	var statements camtDocument
	if err := xml.Unmarshal(read("camt.053.001.08.xml"), &statements); err != nil {
		t.Fatal(err)
	}
	if statements.XMLName.Space != CamtNamespace || statements.Statement.Header.ID == "" || len(statements.Statement.Statements) != len(r.Snapshot.Accounts) {
		t.Fatal("incorrect CAMT namespace, group header or statement count")
	}
	for i, a := range statements.Statement.Statements {
		view := r.Snapshot.Accounts[i]
		if a.Account.ID != accountID(view.Account.ID) || len(a.Balances) != 2 || a.Balances[0].Kind != "OPBD" || a.Balances[1].Kind != "CLBD" {
			t.Fatalf("statement %q is missing account or balance data", a.ID)
		}
		running := new(big.Int)
		for _, item := range a.Entries {
			orig, ok := entries[item.Reference]
			if !ok || orig.Posting.Account != view.Account.ID || item.Amount.Currency != orig.Posting.Amount.Currency ||
				item.Status != "BOOK" || item.Booked != instant(orig.Entry.RecordedAt) || item.ValueDate != instant(orig.Entry.EffectiveAt) ||
				item.Canonical != fmt.Sprintf("transaction=%d;sequence=%d;side=%d;original=%d", orig.Entry.Transaction, orig.Entry.CommitSequence, orig.Posting.Side, orig.Entry.Original) {
				t.Fatalf("Camt entry lost canonical detail: %+v", item)
			}
			n := minorXML(t, c, item.Amount)
			if n.Cmp(big.NewInt(orig.Posting.Amount.Minor)) != 0 {
				t.Fatal("XML changed money")
			}
			if item.Direction == "DBIT" {
				n.Neg(n)
			}
			running.Add(running, n)
			delete(entries, item.Reference)
		}
		n := minorXML(t, c, a.Balances[1].Amount)
		if a.Balances[1].Direction == "DBIT" {
			n.Neg(n)
		}
		if running.Cmp(n) != 0 {
			t.Fatalf("statement %q does not reconcile", a.ID)
		}
	}
	if len(entries) != 0 {
		t.Fatalf("missing %d statement entries", len(entries))
	}
	checkPain(t, c, read("pain.001.001.09.xml"), r)
	checkCards(t, c, read, r)
	if _, err := ExportRun(dir, r, nil, c); err == nil {
		t.Fatal("overwrote previous export directory")
	}
}

func minorXML(t *testing.T, c Catalog, a xmlAmount) *big.Int {
	t.Helper()
	s := c[a.Currency]
	r, ok := new(big.Rat).SetString(a.Value)
	if !ok {
		t.Fatalf("invalid amount %q", a.Value)
	}
	r.Mul(r, new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(s.Digits)), nil)))
	if !r.IsInt() {
		t.Fatal("noninteger converted minor unit")
	}
	return r.Num()
}

func checkPain(t *testing.T, c Catalog, data []byte, r agents.Result) {
	t.Helper()
	var doc painDocument
	if err := xml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	n := 0
	txs := make(map[string]agents.Event)
	for _, e := range r.Events {
		switch e.Activity {
		case agents.Payroll, agents.PeerToPeer, agents.CashSweep, agents.CrossBorder:
			if e.Outcome == domain.Cleared {
				txs[ref("T", uint64(e.Transaction))] = e
				n++
			}
		}
	}
	if doc.XMLName.Space != PainNamespace || doc.Initiation.Header.Count != n || len(doc.Initiation.Payments) != n {
		t.Fatal("unexpected pain namespace or grouping")
	}
	for _, p := range doc.Initiation.Payments {
		e, ok := txs[p.Credit.EndToEnd]
		if !ok || p.Credit.Instruction != ref("J", uint64(e.Entry)) || p.Method != "TRF" {
			t.Fatalf("bad payment projection %+v", p)
		}
		var a xmlAmount
		if e.Quote != nil {
			if p.Credit.Equivalent == nil || p.Credit.Instructed != nil || p.Credit.Equivalent.Target != e.Quote.Target.Currency {
				t.Fatal("FX instruction lost its target currency")
			}
			if p.Credit.Quote == nil || p.Credit.Quote.Rate != e.Quote.Rate ||
				p.Credit.Quote.SettledTarget.Currency != e.Quote.Target.Currency ||
				minorXML(t, c, p.Credit.Quote.SettledTarget).Cmp(big.NewInt(e.Quote.Target.Minor)) != 0 {
				t.Fatal("FX instruction lost booked quote")
			}
			a = p.Credit.Equivalent.Amount
		} else {
			if p.Credit.Instructed == nil || p.Credit.Equivalent != nil {
				t.Fatal("invalid payment amount choice")
			}
			a = *p.Credit.Instructed
		}
		if a.Currency != e.Principal.Currency || minorXML(t, c, a).Cmp(big.NewInt(e.Principal.Minor)) != 0 {
			t.Fatal("pain changed principal")
		}
		delete(txs, p.Credit.EndToEnd)
	}
}

func checkCards(t *testing.T, c Catalog, read func(string) []byte, r agents.Result) {
	t.Helper()
	var index []CardIndex
	if err := json.Unmarshal(read("card-index.json"), &index); err != nil {
		t.Fatal(err)
	}
	expected := make(map[domain.TransactionID]agents.Event)
	for _, e := range r.Events {
		if e.Activity == agents.Card && e.Outcome == domain.Cleared {
			expected[e.Transaction] = e
		}
	}
	for _, x := range []struct {
		name string
		enc  BitmapEncoding
	}{{"card-ascii.dat", HexBitmap}, {"card-binary.dat", BinaryBitmap}} {
		matched := make(map[domain.TransactionID]int)
		raw := read(x.name)
		for _, row := range index {
			msg, rest, err := ReadCardFrame(raw)
			if err != nil {
				t.Fatal(err)
			}
			got, err := DecodeCard(msg, c, x.enc, row.Transmission.Year())
			if err != nil {
				t.Fatal(err)
			}
			event, ok := expected[row.Transaction]
			if !ok || got.Amount != event.Principal || got.Type != row.MTI || !got.Transmission.Equal(row.Transmission) {
				t.Fatalf("unexpected indexed ISO 8583 message %+v %+v", row, got)
			}
			if got.Type == Financial {
				if row.Journal != event.Entry {
					t.Fatal("clearing message lacks canonical journal")
				}
				matched[row.Transaction]++
			}
			raw = rest
		}
		if len(raw) != 0 || len(matched) != len(expected) || len(index) != 2*len(expected) {
			t.Fatal("unexpected number of messages or frames")
		}
		for id, n := range matched {
			if n != 1 {
				t.Fatalf("duplicate clearing message for %d", id)
			}
		}
	}
}

func TestCardHistoryReversalUsesOriginalFinancialMessage(t *testing.T) {
	r, err := agents.RunWithChaos(agents.Config{Seed: 42, StartYear: 2024, Years: 1},
		chaos.Config{Seed: 3, Weights: map[chaos.Fault]uint16{chaos.Freeze: 10000}})
	if err != nil {
		t.Fatal(err)
	}
	var reference domain.JournalEntry
	for _, j := range r.Snapshot.Journals {
		if j.Kind == domain.Reversal {
			reference = j
			break
		}
	}
	ascii, _, rows, err := EncodeCardHistory(r.Snapshot, SimulationCatalog(), []domain.TransactionID{reference.Transaction})
	if err != nil || len(rows) != 3 || rows[2].MTI != CardReversal || rows[2].Journal != reference.ID {
		t.Fatalf("reversal projection %+v %v", rows, err)
	}
	messages := make([]CardMessage, 0, 3)
	for _, row := range rows {
		payload, rest, err := ReadCardFrame(ascii)
		if err != nil {
			t.Fatal(err)
		}
		m, err := DecodeCard(payload, SimulationCatalog(), HexBitmap, row.Transmission.Year())
		if err != nil {
			t.Fatal(err)
		}
		messages = append(messages, m)
		ascii = rest
	}
	prefix := string(Financial) + pad(strconv.FormatUint(uint64(messages[1].STAN), 10), 6) + messages[1].Transmission.Format("0102150405")
	if !strings.HasPrefix(messages[2].OriginalData, prefix) || messages[2].Amount != messages[1].Amount || len(ascii) != 0 {
		t.Fatal("reversal lost original financial linkage")
	}
}
