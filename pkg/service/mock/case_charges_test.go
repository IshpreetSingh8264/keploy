package mock

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml/mapdb"
	"go.keploy.io/server/v3/pkg/platform/yaml/mockdb"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// The spans of a recorded run of a shop paid by a provider's callback: the
// test's GET waits for the order to be paid while the callback's handler
// looks the charge up. Which of the two began first varies from run to run.
const (
	lookupCall    = "GET 127.0.0.1:18111/v1/charges/ch_1"
	sharedSession = "call mock-2 (PostgresV2) is charged to each of the cases get-orders-o-1-paid-1, post-webhooks-pay-1 in case_mocks: it was made while all of them were in flight, and the time alone cannot tell which one's handler made it"
	sharedLookup  = "call mock-1 (" + lookupCall + ") is charged to each of the cases get-orders-o-1-paid-1, post-webhooks-pay-1 in case_mocks: it was made while all of them were in flight, and the time alone cannot tell which one's handler made it"
)

// A call made while two cases of a test were in flight is charged to each
// of them, whichever began first, and the record says so once, with the call
// and the cases. A call made while one case was in flight is that case's
// alone, said nowhere.
func TestACallMadeWhileTwoCasesWereInFlightIsChargedToEach(t *testing.T) {
	at := func(us int) time.Time { return runnerT0.Add(time.Duration(us) * time.Microsecond) }
	windows := []models.ScopeWindow{{Name: paidTest, Start: at(0), End: at(200_000)}}
	place := capturedMock{name: "post-orders-1", ts: at(10_000), end: at(26_000), call: "POST /orders"}
	charge := capturedMock{name: "mock-0", kind: string(models.HTTP), ts: at(15_000), call: "POST 127.0.0.1:18111/v1/charges"}
	for _, tc := range []struct {
		name           string
		paid, callback capturedMock
		lookup         time.Time
	}{
		{
			// As recorded: the GET began 0.37 ms before the callback.
			name:     "the GET first",
			paid:     capturedMock{name: "get-orders-o-1-paid-1", ts: at(27_997), end: at(58_258), call: "GET /orders/o-1/paid"},
			callback: capturedMock{name: "post-webhooks-pay-1", ts: at(28_363), end: at(58_307), call: payCall},
			lookup:   at(53_264),
		},
		{
			// As recorded: the callback came first, and the GET began and
			// ended inside its span, before its handler's lookup.
			name:     "the callback first",
			paid:     capturedMock{name: "get-orders-o-1-paid-1", ts: at(33_000), end: at(52_000), call: "GET /orders/o-1/paid"},
			callback: capturedMock{name: "post-webhooks-pay-1", ts: at(28_000), end: at(60_000), call: payCall},
			lookup:   at(50_000),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lookup := capturedMock{name: "mock-1", kind: string(models.HTTP), ts: tc.lookup, call: lookupCall}
			// A call with no HTTP call to name is named by its kind.
			session := capturedMock{name: "mock-2", kind: string(models.PostgresV2), ts: tc.lookup.Add(time.Microsecond)}
			mocks := []capturedMock{charge, lookup, session}
			got := correlateCases(windows, mocks, []capturedMock{place, tc.paid, tc.callback}, nil)
			require.Equal(t, map[string][]string{
				"post-orders-1":         {"mock-0"},
				"get-orders-o-1-paid-1": {"mock-1", "mock-2"},
				"post-webhooks-pay-1":   {"mock-1", "mock-2"},
			}, got[paidTest].CaseMocks)
			// The same lines each time: the cases come from maps.
			for range 20 {
				require.Equal(t, []string{sharedLookup, sharedSession}, sharedCharges(got, mocks))
			}
		})
	}
	require.Empty(t, sharedCharges(correlateCases(windows, []capturedMock{charge}, []capturedMock{place}, nil), []capturedMock{charge}),
		"a call one case holds is said nowhere")
}

// Recorded, a callback's lookup made while the test's GET was in flight too
// is in both cases' case_mocks, and keploy record says why at INFO.
func TestRecordChargesACallToEachCaseInFlightAndSaysSo(t *testing.T) {
	provenOn(t)
	dir := t.TempDir()
	w := mark(paidTest, 0, 200*time.Millisecond)
	w.Outcome, w.Dir = models.ScopeOutcomePassed, "pay"
	instr := newRunnerInstr(t, w)
	paid := httpCase("get-orders-o-1-paid-1", "GET", "http://localhost:8080/orders/o-1/paid", 200, "{}", runnerT0.Add(28*time.Millisecond))
	paid.HTTPResp.Timestamp = runnerT0.Add(58 * time.Millisecond)
	callback := httpCase("post-webhooks-pay-1", "POST", "http://localhost:8080/webhooks/pay", 200, "ok", runnerT0.Add(29*time.Millisecond))
	callback.HTTPResp.Timestamp = runnerT0.Add(59 * time.Millisecond)
	instr.incoming = []*models.TestCase{paid, callback}
	instr.mocks = []*models.Mock{httpMockAt("GET", "http://127.0.0.1:18111/v1/charges/ch_1", runnerT0.Add(53*time.Millisecond))}
	cfg := instrConfig(instr.composeInstr, utils.Native, "go test -p 1 -json ./...")
	cfg.Path = dir
	withRequests(cfg)
	core, logs := observer.New(zap.InfoLevel)
	svc := New(zap.New(core), instr, mockdb.New(zap.NewNop(), dir, ""), mapdb.New(zap.NewNop(), dir, ""), nil, nil, cfg)
	svc.(TestDBSetter).SetTestDB(&memTestDB{})
	require.NoError(t, svc.Record(context.Background()))

	mp, err := mapdb.New(zap.NewNop(), dir, "").GetMapping(context.Background(), "set")
	require.NoError(t, err)
	require.Len(t, mp.TestCases, 1)
	require.Equal(t, map[string][]string{"get-orders-o-1-paid-1": {"mock-0"}, "post-webhooks-pay-1": {"mock-0"}}, mp.TestCases[0].CaseMocks)
	said := logs.FilterMessage("call mock-0 (" + lookupCall + ") is charged to each of the cases get-orders-o-1-paid-1, post-webhooks-pay-1 in case_mocks: it was made while all of them were in flight, and the time alone cannot tell which one's handler made it").All()
	require.Len(t, said, 1, "the record says the shared charge once: %v", logs.All())
	require.Equal(t, zap.InfoLevel, said[0].Level)
}
