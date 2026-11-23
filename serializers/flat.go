package serializers

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"hash"
	"strconv"
	"strings"
	"time"

	"ssfd-engine/domain"
	"ssfd-engine/ledger"
)

type CodePage string

const (
	ASCII          CodePage = "ascii"
	CP037          CodePage = "cp037"
	CP500          CodePage = "cp500"
	FlatRecordSize          = 200
	flatTime                = "2006-01-02T15:04:05.000000000Z"
	amountOffset            = 135
	amountWidth             = 19
)

func (p CodePage) table() (string, error) {
	switch p {
	case ASCII:
		return "", nil
	case CP037:
		return cp037Printable, nil
	case CP500:
		return cp500Printable, nil
	default:
		return "", fmt.Errorf("unsupported code page %q", p)
	}
}

type FlatDetail struct {
	Entry         domain.EntryID
	Sequence      uint64
	Transaction   domain.TransactionID
	Original      domain.EntryID
	Account       domain.AccountID
	Position      uint64
	Currency      domain.Currency
	Kind          domain.EntryKind
	DebitPositive int64
	EffectiveAt   time.Time
}

type FlatBatch struct {
	Book    domain.BookID
	Details []FlatDetail
}

func EncodeFlat(snapshot ledger.Snapshot, c Catalog, page CodePage) ([]byte, error) {
	m, err := prepare(snapshot, c)
	if err != nil {
		return nil, err
	}
	return m.flat().Marshal(page)
}

func (m model) flat() FlatBatch {
	b := FlatBatch{Book: m.snapshot.Book}
	for _, e := range m.snapshot.Journals {
		for pos, p := range e.Postings {
			n := p.Amount.Minor
			if p.Side == domain.Credit {
				n = -n
			}
			b.Details = append(b.Details, FlatDetail{e.ID, e.CommitSequence, e.Transaction, e.Original, p.Account,
				uint64(pos + 1), p.Amount.Currency, e.Kind, n, e.EffectiveAt.UTC()})
		}
	}
	return b
}

func (b FlatBatch) Marshal(page CodePage) ([]byte, error) {
	tab, err := page.table()
	if err != nil {
		return nil, err
	}
	if b.Book == 0 {
		return nil, fmt.Errorf("missing flat-file book")
	}
	header, err := encodeRecord(fmt.Sprintf("H[SSFD1]%020d", b.Book), tab)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	out.Write(header)
	global := sha256.New()
	currencies := make(map[domain.Currency]hash.Hash)
	totals := make(map[domain.Currency]*total)
	var previous FlatDetail
	journalTotals := make(map[domain.Currency]*total)
	closed := make(map[domain.EntryID]domain.TransactionID)
	check := func() error {
		if previous.Sequence == 0 {
			return nil
		}
		if previous.Position < 2 {
			return fmt.Errorf("journal %d has fewer than two postings", previous.Entry)
		}
		for _, j := range sortedTotals(journalTotals) {
			if j.DebitMinor != j.CreditMinor {
				return fmt.Errorf("unbalanced flat journal %d in %s", previous.Entry, j.Currency)
			}
		}
		closed[previous.Entry] = previous.Transaction
		clear(journalTotals)
		return nil
	}
	for _, d := range b.Details {
		if d.Entry == 0 || d.Sequence == 0 || d.Transaction == 0 || d.Account == 0 || d.Position == 0 || d.Position > 9999999999 ||
			d.DebitPositive == 0 || d.EffectiveAt.IsZero() || d.Kind < domain.Booking || d.Kind > domain.Adjustment ||
			!validFlatCode(d.Currency) || d.Sequence < previous.Sequence ||
			(d.Sequence == previous.Sequence && (d.Entry != previous.Entry || d.Position != previous.Position+1 ||
				d.Transaction != previous.Transaction || d.Original != previous.Original || d.Kind != previous.Kind || !d.EffectiveAt.Equal(previous.EffectiveAt))) ||
			(d.Sequence > previous.Sequence && d.Position != 1) {
			return nil, fmt.Errorf("invalid detail at sequence %d position %d", d.Sequence, d.Position)
		}
		if d.Sequence > previous.Sequence {
			if err := check(); err != nil {
				return nil, err
			}
			if closed[d.Entry] != 0 || (d.Kind == domain.Booking && d.Original != 0) ||
				(d.Kind != domain.Booking && (d.Original == 0 || closed[d.Original] != d.Transaction)) {
				return nil, fmt.Errorf("invalid flat journal identity or original reference")
			}
		}
		previous = d
		stamp := d.EffectiveAt.UTC().Format(flatTime)
		if len(stamp) != 30 {
			return nil, fmt.Errorf("invalid effective timestamp")
		}
		raw := fmt.Sprintf("D%020d%020d%020d%020d%020d%020d%010d%s%d%s%s",
			b.Book, d.Entry, d.Sequence, d.Transaction, d.Original, d.Account, d.Position, d.Currency, d.Kind, strings.Repeat("0", 19), stamp)
		wire, err := encodeRecord(raw, tab)
		if err != nil {
			return nil, err
		}
		copy(wire[amountOffset:amountOffset+amountWidth], zoned(d.DebitPositive, tab != ""))
		out.Write(wire)
		global.Write(wire)
		if currencies[d.Currency] == nil {
			currencies[d.Currency] = sha256.New()
		}
		currencies[d.Currency].Write(wire)
		accumulate(totals, d.Currency, d.DebitPositive)
		accumulate(journalTotals, d.Currency, d.DebitPositive)
	}
	if err := check(); err != nil {
		return nil, err
	}
	for _, v := range sortedTotals(totals) {
		if len(v.DebitMinor) > 31 || len(v.CreditMinor) > 31 {
			return nil, fmt.Errorf("trailer monetary total exceeds 31 digits")
		}
		trailer := fmt.Sprintf("T%s%020d%s%s%X", v.Currency, v.Count, pad(v.DebitMinor, 31), pad(v.CreditMinor, 31), currencies[v.Currency].Sum(nil))
		wire, err := encodeRecord(trailer, tab)
		if err != nil {
			return nil, err
		}
		out.Write(wire)
	}
	trailer, err := encodeRecord(fmt.Sprintf("T***%020d%s%X", len(b.Details), strings.Repeat("0", 62), global.Sum(nil)), tab)
	if err != nil {
		return nil, err
	}
	out.Write(trailer)
	return out.Bytes(), nil
}

func validFlatCode(c domain.Currency) bool {
	return len(c) == 3 && c[0] >= 'A' && c[0] <= 'Z' && c[1] >= 'A' && c[1] <= 'Z' && c[2] >= 'A' && c[2] <= 'Z'
}

func pad(s string, width int) string { return strings.Repeat("0", width-len(s)) + s }

func encodeRecord(raw, table string) ([]byte, error) {
	if len(raw) > FlatRecordSize {
		return nil, fmt.Errorf("flat record exceeds width")
	}
	raw += strings.Repeat(" ", FlatRecordSize-len(raw))
	b := []byte(raw)
	for i, c := range b {
		if c < 32 || c > 126 {
			return nil, fmt.Errorf("unsupported flat-file character")
		}
		if table != "" {
			b[i] = table[c-32]
		}
	}
	return b, nil
}

const positivePunch = "{ABCDEFGHI"
const negativePunch = "}JKLMNOPQR"

func zoned(n int64, ebcdic bool) []byte {
	s := strconv.FormatInt(n, 10)
	if n < 0 {
		s = s[1:]
	}
	out := []byte(pad(s, amountWidth))
	last := len(out) - 1
	digit := out[last] - '0'
	if ebcdic {
		for i := range out {
			out[i] = 0xF0 + (out[i] - '0')
		}
		out[last] = 0xC0 + digit
		if n < 0 {
			out[last] = 0xD0 + digit
		}
	} else if n < 0 {
		out[last] = negativePunch[digit]
	} else {
		out[last] = positivePunch[digit]
	}
	return out
}

func parseZoned(in []byte, ebcdic bool) (int64, error) {
	d := append([]byte(nil), in...)
	idx := len(d) - 1
	negative := false
	if ebcdic {
		for i, b := range d[:idx] {
			if b < 0xF0 || b > 0xF9 {
				return 0, fmt.Errorf("bad EBCDIC zoned digit")
			}
			d[i] = '0' + b - 0xF0
		}
		b := d[idx]
		if b >= 0xC0 && b <= 0xC9 {
			d[idx] = '0' + b - 0xC0
		} else if b >= 0xD0 && b <= 0xD9 {
			negative = true
			d[idx] = '0' + b - 0xD0
		} else {
			return 0, fmt.Errorf("bad EBCDIC zoned sign")
		}
	} else {
		n := strings.IndexByte(positivePunch, d[idx])
		if n < 0 {
			negative = true
			n = strings.IndexByte(negativePunch, d[idx])
		}
		if n < 0 {
			return 0, fmt.Errorf("bad ASCII overpunch")
		}
		d[idx] = '0' + byte(n)
	}
	if !digits(string(d), amountWidth) {
		return 0, fmt.Errorf("bad zoned digits")
	}
	s := string(d)
	if negative {
		s = "-" + s
	}
	return strconv.ParseInt(s, 10, 64)
}

// DecodeFlat verifies framing and reconstructs all details. Re-encoding
// verifies canonical signs, padding, counts and every trailer hash.
func DecodeFlat(raw []byte, page CodePage) (FlatBatch, error) {
	tab, err := page.table()
	if err != nil {
		return FlatBatch{}, err
	}
	if len(raw)%FlatRecordSize != 0 || len(raw) < 2*FlatRecordSize {
		return FlatBatch{}, fmt.Errorf("invalid flat batch length")
	}
	var b FlatBatch
	var inverse [256]byte
	for i, v := range []byte(tab) {
		inverse[v] = byte(i + 32)
	}
	for off := 0; off < len(raw); off += FlatRecordSize {
		wire := raw[off : off+FlatRecordSize]
		record := append([]byte(nil), wire...)
		if tab != "" {
			for j, v := range record {
				record[j] = inverse[v]
			}
		}
		if off == 0 {
			if string(record[:8]) != "H[SSFD1]" {
				return FlatBatch{}, fmt.Errorf("invalid flat header")
			}
			id, err := strconv.ParseUint(string(record[8:28]), 10, 64)
			if err != nil {
				return FlatBatch{}, err
			}
			b.Book = domain.BookID(id)
			continue
		}
		if record[0] == 'T' {
			continue
		}
		if record[0] != 'D' {
			return FlatBatch{}, fmt.Errorf("unknown flat record")
		}
		numbers := make([]uint64, 7)
		for j := range numbers {
			start, end := 1+j*20, 21+j*20
			if j == 6 {
				end = 131
			}
			if !digits(string(record[start:end]), end-start) {
				return FlatBatch{}, fmt.Errorf("bad detail identifier")
			}
			numbers[j], err = strconv.ParseUint(string(record[start:end]), 10, 64)
			if err != nil {
				return FlatBatch{}, err
			}
		}
		n, err := parseZoned(wire[amountOffset:amountOffset+amountWidth], tab != "")
		if err != nil {
			return FlatBatch{}, err
		}
		stamp, err := time.Parse(flatTime, string(record[amountOffset+amountWidth:amountOffset+amountWidth+30]))
		if err != nil {
			return FlatBatch{}, err
		}
		b.Details = append(b.Details, FlatDetail{domain.EntryID(numbers[1]), numbers[2], domain.TransactionID(numbers[3]),
			domain.EntryID(numbers[4]), domain.AccountID(numbers[5]), numbers[6], domain.Currency(record[131:134]),
			domain.EntryKind(record[134] - '0'), n, stamp})
	}
	expected, err := b.Marshal(page)
	if err != nil {
		return FlatBatch{}, err
	}
	if !bytes.Equal(raw, expected) {
		return FlatBatch{}, fmt.Errorf("flat detail, trailer totals or hash mismatch")
	}
	return b, nil
}
