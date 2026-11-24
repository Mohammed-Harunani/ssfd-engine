package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func performRequest(t *testing.T, handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func decodeResponse(t *testing.T, rec *httptest.ResponseRecorder, dst any) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d: %s", rec.Code, rec.Body)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), dst); err != nil {
		t.Fatalf("decode JSON response: %v", err)
	}
}

func TestEmbeddedConsoleAndHealth(t *testing.T) {
	handler := newAPI().routes()

	page := performRequest(t, handler, http.MethodGet, "/", "")
	if page.Code != http.StatusOK ||
		!strings.HasPrefix(page.Header().Get("Content-Type"), "text/html") ||
		!strings.Contains(page.Body.String(), "Run Live Go Simulation") {
		t.Fatalf("embedded console missing or invalid: HTTP %d", page.Code)
	}

	var health struct {
		Status            string `json:"status"`
		SimulationVersion string `json:"simulationVersion"`
		RandomAlgorithm   string `json:"randomAlgorithm"`
	}
	decodeResponse(t, performRequest(t, handler, http.MethodGet, "/api/health", ""), &health)
	if health.Status != "ready" || health.SimulationVersion == "" || health.RandomAlgorithm == "" {
		t.Fatalf("unexpected health response: %+v", health)
	}
}

func TestSimulateAggressiveSeed42(t *testing.T) {
	handler := newAPI().routes()
	const body = `{"seed":42,"startYear":2024,"years":1,"chaosProfile":"aggressive"}`

	var result struct {
		JournalCount     int               `json:"journalCount"`
		Balanced         bool              `json:"balanced"`
		DiscrepancyLabel string            `json:"discrepancyLabel"`
		TrialBalance     []trialBalanceRow `json:"trialBalance"`
		Chaos            chaosSummary      `json:"chaos"`
		WireSamples      wireSamples       `json:"wireSamples"`
	}
	decodeResponse(t, performRequest(t, handler, http.MethodPost, "/api/simulate", body), &result)

	if result.JournalCount != 6268 || !result.Balanced || result.DiscrepancyLabel != "£0.00 / $0.00" {
		t.Fatalf("unexpected reconciliation: %d journals, balanced=%t, difference=%q",
			result.JournalCount, result.Balanced, result.DiscrepancyLabel)
	}
	if len(result.TrialBalance) != 2 ||
		result.TrialBalance[0].Currency != "GBP" || result.TrialBalance[1].Currency != "USD" {
		t.Fatalf("unexpected currencies: %+v", result.TrialBalance)
	}
	for _, row := range result.TrialBalance {
		if row.DebitsMinor != row.CreditsMinor || row.DiscrepancyMinor != "0" {
			t.Fatalf("unbalanced currency row: %+v", row)
		}
	}
	if result.Chaos.InjectedScenarios != 2197 ||
		result.Chaos.BlockedInvalidCommands != 2195 ||
		result.Chaos.DeduplicatedRetries != 366 ||
		result.Chaos.DelayedSettlements != 365 {
		t.Fatalf("unexpected chaos counts: %+v", result.Chaos)
	}

	wire := result.WireSamples
	if !strings.Contains(wire.Pain001XML, "urn:iso:std:iso:20022:tech:xsd:pain.001.001.09") ||
		!strings.HasPrefix(wire.ISO8583ASCII, "0200"+wire.ISO8583PrimaryHex) ||
		len(wire.ISO8583PrimaryHex) != 16 ||
		!strings.HasPrefix(wire.CP037Hex, "00000000  c8 ba") ||
		!strings.HasPrefix(wire.CP037ASCIIMirror, "H[SSFD1]") {
		t.Fatal("one or more wire samples are missing or invalid")
	}
}

func TestMutationEvaluation(t *testing.T) {
	handler := newAPI().routes()
	var result struct {
		Seeds        []uint64 `json:"seeds"`
		CasesPerSeed int      `json:"casesPerSeed"`
		MutantCount  int      `json:"mutantCount"`
		Scores       []struct {
			Strategy   string `json:"strategy"`
			Kills      int    `json:"kills"`
			MutantRuns int    `json:"mutantRuns"`
		} `json:"scores"`
	}
	decodeResponse(t, performRequest(t, handler, http.MethodPost, "/api/mutation-eval", "{}"), &result)

	if len(result.Seeds) != 2 || result.Seeds[0] != 42 || result.Seeds[1] != 43 ||
		result.CasesPerSeed != 192 || result.MutantCount != 13 || len(result.Scores) != 3 {
		t.Fatalf("unexpected evaluation sample: %+v", result)
	}
	want := map[string]int{"naive": 6, "ssfd": 4, "ssfd-chaos": 24}
	for _, score := range result.Scores {
		kills, exists := want[score.Strategy]
		if !exists || score.Kills != kills || score.MutantRuns != 26 {
			t.Fatalf("unexpected strategy result: %+v", score)
		}
		delete(want, score.Strategy)
	}
}

func TestInvalidRequests(t *testing.T) {
	handler := newAPI().routes()
	for _, tc := range []struct {
		name, method, path, body string
		status                   int
	}{
		{"wrong method", "GET", "/api/simulate", "", http.StatusMethodNotAllowed},
		{"missing content type", "POST", "/api/simulate", "", http.StatusUnsupportedMediaType},
		{"missing seed", "POST", "/api/simulate", `{"startYear":2024,"years":1,"chaosProfile":"none"}`, http.StatusBadRequest},
		{"invalid horizon", "POST", "/api/simulate", `{"seed":42,"startYear":2028,"years":2,"chaosProfile":"none"}`, http.StatusBadRequest},
		{"unknown profile", "POST", "/api/simulate", `{"seed":42,"startYear":2024,"years":1,"chaosProfile":"other"}`, http.StatusBadRequest},
		{"unknown JSON field", "POST", "/api/mutation-eval", `{"extra":true}`, http.StatusBadRequest},
		{"oversized body", "POST", "/api/mutation-eval", strings.Repeat(" ", maxJSONBody+1), http.StatusRequestEntityTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := performRequest(t, handler, tc.method, tc.path, tc.body)
			if rec.Code != tc.status {
				t.Fatalf("expected HTTP %d, got %d: %s", tc.status, rec.Code, rec.Body)
			}
		})
	}
}
