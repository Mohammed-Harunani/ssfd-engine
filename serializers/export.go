package serializers

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"ssfd-engine/agents"
	"ssfd-engine/chaos"
	"ssfd-engine/domain"
	"ssfd-engine/ledger"
)

const BundleVersion = "ssfd-export-v1"

type ManifestFile struct {
	Name   string
	Bytes  int
	SHA256 string
}

type Manifest struct {
	Version       string
	Book          domain.BookID
	Sequence      uint64
	Currencies    Catalog
	PostingTotals []PostingTotals
	TrialBalance  []ledger.TrialRow
	Files         []ManifestFile
}

type CardIndex struct {
	Number       int
	MTI          MTI
	Transaction  domain.TransactionID
	Journal      domain.EntryID
	Transmission time.Time
}

type FaultMetadata struct {
	Version string
	Seed    uint64
	Stats   []chaos.FaultStats
}

type artifact struct {
	name string
	data []byte
}

// ExportRun writes a self-contained, deterministic bundle into a new directory.
// The manifest is written last. Consumers should only accept completed bundles.
func ExportRun(dir string, r agents.Result, faults *FaultMetadata, c Catalog) (Manifest, error) {
	if dir == "" {
		return Manifest{}, fmt.Errorf("export directory is empty")
	}
	m, err := prepare(r.Snapshot, c)
	if err != nil {
		return Manifest{}, err
	}
	if !slices.Equal(m.trial, r.TrialBalance) {
		return Manifest{}, fmt.Errorf("supplied trial balance disagrees with canonical replay")
	}
	a, err := m.artifacts(r, faults, c)
	if err != nil {
		return Manifest{}, err
	}
	manifest := Manifest{Version: BundleVersion, Book: r.Snapshot.Book, Sequence: r.Snapshot.Sequence,
		Currencies: c, PostingTotals: m.totals, TrialBalance: m.trial}
	for _, f := range a {
		manifest.Files = append(manifest.Files, ManifestFile{f.name, len(f.data), fmt.Sprintf("%x", sha256.Sum256(f.data))})
	}
	data, err := jsonFile(manifest)
	if err != nil {
		return Manifest{}, err
	}
	a = append(a, artifact{"manifest.json", data})
	if err := writeArtifacts(dir, a); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func (m model) artifacts(r agents.Result, faults *FaultMetadata, c Catalog) ([]artifact, error) {
	var transfers []domain.TransactionID
	var cards []domain.TransactionID
	for _, event := range r.Events {
		if event.Entry == 0 || event.Outcome != domain.Cleared {
			continue
		}
		j := m.entries[event.Entry]
		if j.Transaction != event.Transaction || j.Kind != domain.Booking {
			return nil, fmt.Errorf("activity %d has no matching committed booking", event.Transaction)
		}
		switch event.Activity {
		case agents.Payroll, agents.PeerToPeer, agents.CashSweep, agents.CrossBorder:
			transfers = append(transfers, event.Transaction)
		case agents.Card:
			cards = append(cards, event.Transaction)
		}
	}
	// Preserve the full canonical result and optional scenario metadata.
	canonical, err := jsonFile(struct {
		Run   agents.Result
		Chaos *FaultMetadata `json:",omitempty"`
	}{r, faults})
	if err != nil {
		return nil, err
	}
	artifacts := []artifact{{"canonical.json", canonical}}
	if len(transfers) > 0 {
		pain, err := m.pain(c, transfers)
		if err != nil {
			return nil, err
		}
		artifacts = append(artifacts, artifact{"pain.001.001.09.xml", pain})
	}
	camt, err := m.camt(c)
	if err != nil {
		return nil, err
	}
	artifacts = append(artifacts, artifact{"camt.053.001.08.xml", camt})
	hex, bin, index, err := m.cardStreams(cards, c)
	if err != nil {
		return nil, err
	}
	cardIndex, err := jsonFile(index)
	if err != nil {
		return nil, err
	}
	artifacts = append(artifacts, artifact{"card-ascii.dat", hex}, artifact{"card-binary.dat", bin}, artifact{"card-index.json", cardIndex})
	b := m.flat()
	for _, p := range []CodePage{ASCII, CP037, CP500} {
		payload, err := b.Marshal(p)
		if err != nil {
			return nil, err
		}
		artifacts = append(artifacts, artifact{"ledger-" + string(p) + ".dat", payload})
	}
	return artifacts, nil
}

func jsonFile(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

func writeArtifacts(dir string, artifacts []artifact) error {
	if err := os.Mkdir(dir, 0755); err != nil {
		return fmt.Errorf("create new export directory: %w", err)
	}
	var installed []string
	for _, a := range artifacts {
		path := filepath.Join(dir, a.name)
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
		if err == nil {
			installed = append(installed, path)
			_, err = f.Write(a.data)
			if closeErr := f.Close(); err == nil {
				err = closeErr
			}
		}
		if err != nil {
			for _, p := range installed {
				_ = os.Remove(p)
			}
			_ = os.Remove(dir)
			return fmt.Errorf("write %s: %w", a.name, err)
		}
	}
	return nil
}

type cardState struct {
	stan      uint32
	financial CardMessage
	original  domain.EntryID
}

// EncodeCardHistory projects explicitly selected card transaction lifecycles.
// Each stream uses SSFD length framing. The index records the DE7 year and
// exact canonical timestamp, including any subsecond precision.
func EncodeCardHistory(s ledger.Snapshot, c Catalog, ids []domain.TransactionID) ([]byte, []byte, []CardIndex, error) {
	m, err := prepare(s, c)
	if err != nil {
		return nil, nil, nil, err
	}
	return m.cardStreams(ids, c)
}

func (m model) cardStreams(ids []domain.TransactionID, c Catalog) ([]byte, []byte, []CardIndex, error) {
	selected := make(map[domain.TransactionID]*cardState, len(ids))
	for _, id := range ids {
		if selected[id] != nil || m.transactions[id].ID == 0 || m.transactions[id].Kind != domain.SingleCurrency {
			return nil, nil, nil, fmt.Errorf("invalid selected card transaction %d", id)
		}
		selected[id] = new(cardState)
	}
	var text, bin bytes.Buffer
	rows := make([]CardIndex, 0)
	var used uint32
	for _, ev := range m.snapshot.TransactionEvents {
		s := selected[ev.Transaction]
		if s == nil {
			continue
		}
		money := m.transactions[ev.Transaction].Principal
		msg := CardMessage{ProcessingCode: "000000", Amount: money, Transmission: ev.RecordedAt.UTC().Truncate(time.Second)}
		switch {
		case ev.From == domain.Pending && ev.To == domain.Authorized:
			used = used%999999 + 1
			s.stan = used
			msg.Type = Authorization
		case ev.From == domain.Authorized && ev.To == domain.Cleared:
			j := m.entries[ev.Journal]
			if s.stan == 0 || j.Kind != domain.Booking || j.Transaction != ev.Transaction || !m.cardPrincipal(j) {
				return nil, nil, nil, fmt.Errorf("card booking lacks authorization or journal")
			}
			msg.Type = Financial
			s.original = ev.Journal
		case ev.To == domain.Reversed:
			e := m.entries[ev.Journal]
			if e.Kind != domain.Reversal { // A terminal partial-refund path has no full reversal message in this profile.
				continue
			}
			if e.Original != s.original || s.financial.Type != Financial ||
				!reverses(e, m.entries[s.original]) {
				return nil, nil, nil, fmt.Errorf("card reversal does not invert its original booking")
			}
			msg.Type = CardReversal
			msg.OriginalData = fmt.Sprintf("%s%06d%s0000000000100000000002", s.financial.Type, s.stan, s.financial.Transmission.Format("0102150405"))
		default:
			continue
		}
		msg.STAN = s.stan
		if msg.Type == Financial {
			s.financial = msg
		}
		for _, wire := range []struct {
			codec BitmapEncoding
			dst   *bytes.Buffer
		}{{HexBitmap, &text}, {BinaryBitmap, &bin}} {
			payload, err := EncodeCard(msg, c, wire.codec)
			if err != nil {
				return nil, nil, nil, err
			}
			if err := WriteCardFrame(wire.dst, payload); err != nil {
				return nil, nil, nil, err
			}
		}
		rows = append(rows, CardIndex{len(rows) + 1, msg.Type, ev.Transaction, ev.Journal, ev.RecordedAt})
	}
	return text.Bytes(), bin.Bytes(), rows, nil
}

func (m model) cardPrincipal(j domain.JournalEntry) bool {
	t := m.transactions[j.Transaction]
	if len(j.Postings) != 2 || t.Principal.Minor <= 0 {
		return false
	}
	var debit, credit bool
	for _, p := range j.Postings {
		a := m.accounts[p.Account].Account
		if p.Amount != t.Principal || a.Class != domain.Liability {
			return false
		}
		switch p.Side {
		case domain.Debit:
			debit = a.Owner == t.Debtor
		case domain.Credit:
			credit = a.Owner == t.Creditor
		}
	}
	return debit && credit
}

func reverses(reversal, original domain.JournalEntry) bool {
	if reversal.Transaction != original.Transaction || len(reversal.Postings) != len(original.Postings) {
		return false
	}
	for i, p := range original.Postings {
		got := reversal.Postings[i]
		if p.Account != got.Account || p.Amount != got.Amount || p.Side == got.Side {
			return false
		}
	}
	return true
}
