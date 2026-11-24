package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"runtime"
	"runtime/debug"
	"strconv"
	"time"

	"ssfd-engine/agents"
	"ssfd-engine/chaos"
	"ssfd-engine/domain"
	"ssfd-engine/eval"
	"ssfd-engine/ledger"
	"ssfd-engine/prng"
	"ssfd-engine/serializers"
)

const maxJSONBody = 4096

type api struct {
	// Bound concurrent simulation work while keeping health checks available.
	work chan struct{}
}

func newAPI() *api {
	return &api{work: make(chan struct{}, 1)}
}

func (a *api) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", serveConsole)
	mux.HandleFunc("GET /api/health", a.health)
	mux.HandleFunc("POST /api/simulate", a.simulate)
	mux.HandleFunc("POST /api/mutation-eval", a.mutationEval)
	return mux
}

func (a *api) health(w http.ResponseWriter, _ *http.Request) {
	result := map[string]any{
		"status":            "ready",
		"apiVersion":        "1",
		"simulationVersion": agents.Version,
		"chaosVersion":      chaos.Version,
		"randomAlgorithm":   prng.Version,
		"bundleVersion":     serializers.BundleVersion,
		"goVersion":         runtime.Version(),
		"os":                runtime.GOOS,
		"arch":              runtime.GOARCH,
	}
	if build, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range build.Settings {
			switch setting.Key {
			case "vcs.revision":
				result["revision"] = setting.Value
			case "vcs.modified":
				result["modified"] = setting.Value == "true"
			}
		}
	}
	writeJSON(w, http.StatusOK, result)
}

type simulationRequest struct {
	Seed         *int64 `json:"seed"`
	StartYear    int    `json:"startYear"`
	Years        int    `json:"years"`
	ChaosProfile string `json:"chaosProfile"`
}

func (req simulationRequest) validate() error {
	if req.Seed == nil {
		return errors.New("seed must be a signed 64-bit integer")
	}
	if req.StartYear < 2019 || req.StartYear > 2028 {
		return errors.New("startYear must be between 2019 and 2028")
	}
	if req.Years < 1 || req.Years > 5 || req.Years > 2029-req.StartYear {
		return errors.New("years must be 1–5 and the run must end by December 2028")
	}
	switch req.ChaosProfile {
	case "none", "moderate", "aggressive":
		return nil
	default:
		return errors.New("chaosProfile must be none, moderate, or aggressive")
	}
}

func (a *api) simulate(w http.ResponseWriter, r *http.Request) {
	var req simulationRequest
	if !readJSON(w, r, &req) {
		return
	}
	if err := req.validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !a.acquire(w) {
		return
	}
	defer a.release()

	// Preserve all bits when mapping the signed API seed to the engine seed.
	seed := uint64(*req.Seed)
	cfg := agents.Config{Seed: seed, StartYear: req.StartYear, Years: req.Years}
	run, faults, err := runSimulation(cfg, req.ChaosProfile)
	if err != nil {
		internalError(w, "simulation", err)
		return
	}

	catalog := serializers.SimulationCatalog()
	rows, balanced, label, err := formatTrialBalance(run.TrialBalance, catalog)
	if err != nil {
		internalError(w, "trial balance", err)
		return
	}
	samples, err := makeWireSamples(run, catalog)
	if err != nil {
		internalError(w, "wire serialization", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"seed":             strconv.FormatInt(*req.Seed, 10),
		"engineSeed":       strconv.FormatUint(seed, 10),
		"startYear":        req.StartYear,
		"years":            req.Years,
		"chaosProfile":     req.ChaosProfile,
		"journalCount":     len(run.Snapshot.Journals),
		"trialBalance":     rows,
		"balanced":         balanced,
		"discrepancyLabel": label,
		"chaos":            summarizeFaults(faults),
		"wireSamples":      samples,
	})
}

func runSimulation(cfg agents.Config, profile string) (agents.Result, []chaos.FaultStats, error) {
	if profile == "none" {
		run, err := agents.Run(cfg)
		return run, nil, err
	}
	if profile == "moderate" {
		profile = "standard"
	}
	spec, err := chaos.Profile(profile, cfg.Seed)
	if err != nil {
		return agents.Result{}, nil, err
	}
	run, err := agents.RunWithChaos(cfg, spec)
	return run.Result, run.Chaos, err
}

type trialBalanceRow struct {
	Currency         domain.Currency `json:"currency"`
	DebitsMinor      string          `json:"debitsMinor"`
	CreditsMinor     string          `json:"creditsMinor"`
	DiscrepancyMinor string          `json:"discrepancyMinor"`
	Debits           string          `json:"debits"`
	Credits          string          `json:"credits"`
	Discrepancy      string          `json:"discrepancy"`
}

func formatTrialBalance(rows []ledger.TrialRow, catalog serializers.Catalog) ([]trialBalanceRow, bool, string, error) {
	out := make([]trialBalanceRow, 0, 2)
	balanced := true
	for _, target := range []struct {
		code   domain.Currency
		symbol string
	}{{"GBP", "£"}, {"USD", "$"}} {
		var row ledger.TrialRow
		for _, candidate := range rows {
			if candidate.Currency == target.code {
				row = candidate
				break
			}
		}
		if row.Currency == "" {
			return nil, false, "", fmt.Errorf("missing %s trial balance", target.code)
		}

		exact := [3]string{row.Debits, row.Credits, row.Difference}
		var display [3]string
		for i, raw := range exact {
			minor, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				return nil, false, "", fmt.Errorf("%s amount: %w", target.code, err)
			}
			value, err := catalog.Decimal(target.code, minor)
			if err != nil {
				return nil, false, "", err
			}
			display[i] = target.symbol + value
			if i == 2 && minor != 0 {
				balanced = false
			}
		}
		balanced = balanced && row.Debits == row.Credits
		out = append(out, trialBalanceRow{
			Currency:         target.code,
			DebitsMinor:      row.Debits,
			CreditsMinor:     row.Credits,
			DiscrepancyMinor: row.Difference,
			Debits:           display[0],
			Credits:          display[1],
			Discrepancy:      display[2],
		})
	}
	return out, balanced, out[0].Discrepancy + " / " + out[1].Discrepancy, nil
}

type chaosSummary struct {
	InjectedScenarios      uint64           `json:"injectedScenarios"`
	BlockedInvalidCommands uint64           `json:"blockedInvalidCommands"`
	DeduplicatedRetries    uint64           `json:"deduplicatedRetries"`
	DelayedSettlements     uint64           `json:"delayedSettlements"`
	Faults                 []map[string]any `json:"faults"`
}

func summarizeFaults(stats []chaos.FaultStats) chaosSummary {
	result := chaosSummary{Faults: make([]map[string]any, 0, len(stats))}
	for _, stat := range stats {
		result.InjectedScenarios += stat.Injected
		result.BlockedInvalidCommands += stat.Rejected
		result.DeduplicatedRetries += stat.IdenticalRetries
		result.DelayedSettlements += stat.DelayedSettlements

		result.Faults = append(result.Faults, map[string]any{
			"fault":                  stat.Fault,
			"weightBps":              stat.WeightBPS,
			"eligible":               stat.Eligible,
			"injectedScenarios":      stat.Injected,
			"blockedInvalidCommands": stat.Rejected,
			"deduplicatedRetries":    stat.IdenticalRetries,
			"delayedSettlements":     stat.DelayedSettlements,
			"journals":               stat.Journals,
		})
	}
	return result
}

type wireSamples struct {
	Pain001XML        string `json:"pain001Xml"`
	ISO8583ASCII      string `json:"iso8583Ascii"`
	ISO8583PrimaryHex string `json:"iso8583PrimaryHex"`
	CP037Hex          string `json:"cp037Hex"`
	CP037ASCIIMirror  string `json:"cp037AsciiMirror"`
}

func makeWireSamples(run agents.Result, catalog serializers.Catalog) (wireSamples, error) {
	var payroll, card agents.Event
	for _, event := range run.Events {
		if event.Outcome != domain.Cleared || event.Entry == 0 {
			continue
		}
		switch {
		case event.Activity == agents.Payroll && payroll.Entry == 0:
			payroll = event
		case event.Activity == agents.Card && card.Entry == 0:
			card = event
		}
		if payroll.Entry != 0 && card.Entry != 0 {
			break
		}
	}
	if payroll.Entry == 0 || card.Entry == 0 {
		return wireSamples{}, errors.New("run lacks a booked payroll or card event")
	}

	pain, err := serializers.EncodePain(run.Snapshot, catalog, []domain.TransactionID{payroll.Transaction})
	if err != nil {
		return wireSamples{}, fmt.Errorf("pain.001 sample: %w", err)
	}
	message, err := serializers.EncodeCard(serializers.CardMessage{
		Type:           serializers.Financial,
		ProcessingCode: "000000",
		Amount:         card.Principal,
		Transmission:   card.At.UTC().Truncate(time.Second),
		STAN:           1,
	}, catalog, serializers.HexBitmap)
	if err != nil {
		return wireSamples{}, fmt.Errorf("ISO 8583 sample: %w", err)
	}

	batch, err := payrollBatch(run.Snapshot, payroll.Entry)
	if err != nil {
		return wireSamples{}, err
	}
	ebcdic, err := batch.Marshal(serializers.CP037)
	if err != nil {
		return wireSamples{}, fmt.Errorf("CP037 sample: %w", err)
	}
	ascii, err := batch.Marshal(serializers.ASCII)
	if err != nil {
		return wireSamples{}, fmt.Errorf("ASCII mirror: %w", err)
	}

	var mirror bytes.Buffer
	for record := 0; record < len(ascii); record += serializers.FlatRecordSize {
		mirror.Write(ascii[record : record+serializers.FlatRecordSize])
		mirror.WriteByte('\n')
	}

	return wireSamples{
		Pain001XML:        string(pain),
		ISO8583ASCII:      string(message),
		ISO8583PrimaryHex: string(message[4:20]),
		CP037Hex:          hex.Dump(ebcdic),
		CP037ASCIIMirror:  mirror.String(),
	}, nil
}

func payrollBatch(snapshot ledger.Snapshot, entryID domain.EntryID) (serializers.FlatBatch, error) {
	for _, entry := range snapshot.Journals {
		if entry.ID != entryID {
			continue
		}
		batch := serializers.FlatBatch{Book: snapshot.Book}
		for index, posting := range entry.Postings {
			minor := posting.Amount.Minor
			if posting.Side == domain.Credit {
				minor = -minor
			}
			batch.Details = append(batch.Details, serializers.FlatDetail{
				Entry:         entry.ID,
				Sequence:      entry.CommitSequence,
				Transaction:   entry.Transaction,
				Original:      entry.Original,
				Account:       posting.Account,
				Position:      uint64(index + 1),
				Currency:      posting.Amount.Currency,
				Kind:          entry.Kind,
				DebitPositive: minor,
				EffectiveAt:   entry.EffectiveAt,
			})
		}
		return batch, nil
	}
	return serializers.FlatBatch{}, errors.New("booked payroll event has no journal")
}

func (a *api) mutationEval(w http.ResponseWriter, r *http.Request) {
	var req struct{}
	if !readJSON(w, r, &req) || !a.acquire(w) {
		return
	}
	defer a.release()

	seeds := []uint64{42, 43}
	report, err := eval.Evaluate(seeds, 192)
	if err != nil {
		internalError(w, "mutation evaluation", err)
		return
	}
	labels := map[eval.Strategy]string{
		eval.Naive:      "Naive Random",
		eval.Happy:      "SSFD Happy Path",
		eval.Aggressive: "SSFD + Aggressive Chaos",
	}
	scores := make([]map[string]any, 0, len(report.Summary))
	for _, summary := range report.Summary {
		scores = append(scores, map[string]any{
			"strategy":     summary.Strategy,
			"label":        labels[summary.Strategy],
			"kills":        summary.Kills,
			"mutantRuns":   summary.MutantRuns,
			"scorePercent": summary.ScorePercent,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"seeds":        seeds,
		"casesPerSeed": report.CasesPerSeed,
		"mutantCount":  len(report.Mutants),
		"scores":       scores,
	})
}

func (a *api) acquire(w http.ResponseWriter) bool {
	select {
	case a.work <- struct{}{}:
		return true
	default:
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusServiceUnavailable, "engine busy; retry after the current run")
		return false
	}
}

func (a *api) release() {
	<-a.work
}

func readJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		return false
	}

	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxJSONBody))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body exceeds 4096 bytes")
		} else {
			writeError(w, http.StatusBadRequest, "could not read request body")
		}
		return false
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' || !json.Valid(raw) {
		writeError(w, http.StatusBadRequest, "request body must contain one valid JSON object")
		return false
	}

	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON fields: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Printf("write JSON response: %v", err)
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func internalError(w http.ResponseWriter, action string, err error) {
	log.Printf("%s failed: %v", action, err)
	writeError(w, http.StatusInternalServerError, action+" failed; see server log")
}
