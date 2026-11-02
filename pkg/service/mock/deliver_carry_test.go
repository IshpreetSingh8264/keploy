package mock

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml/mapdb"
	"go.keploy.io/server/v3/pkg/platform/yaml/mockdb"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// httpMockAt is an HTTP mock of a call to method url at at.
func httpMockAt(method, url string, at time.Time) *models.Mock {
	mk := mockAt("", at)
	mk.Spec.HTTPReq.Method, mk.Spec.HTTPReq.URL = models.Method(method), url
	return mk
}

// walkMocks is a mock store that hands WalkTestSetMocks its mocks, or fails
// with err.
type walkMocks struct {
	stubMockDB
	mocks []*models.Mock
	err   error
}

func (w walkMocks) WalkTestSetMocks(_ context.Context, _ string, visit func(*models.Mock, bool, bool)) error {
	for _, mk := range w.mocks {
		visit(mk, true, false)
	}
	return w.err
}

const (
	paidTest  = "pay::TestPaidByCallback"
	payCall   = "POST /webhooks/pay"
	chargeURL = "http://pay.example:18110/v1/charges"
)

var paySign = &models.SignSpec{Scheme: models.SignHMACSHA256Hex, Header: "X-Pay-Signature", SecretEnv: "PAY_WEBHOOK_SECRET"}

// Recorded again, a set keeps a test's accepted deliver: entry, named as the
// new recording names its callback case and trigger call, and says so with
// both names: the replay then sends the callback as before.
func TestAReRecordCarriesADeliverEntryToItsCallsNewNames(t *testing.T) {
	provenOn(t)
	dir := t.TempDir()
	w := mark(paidTest, 0, 10*time.Millisecond)
	w.Outcome, w.Dir = models.ScopeOutcomePassed, "pay"
	record := func(logger *zap.Logger, db *memTestDB, incoming []*models.TestCase, mocks ...*models.Mock) {
		t.Helper()
		instr := newRunnerInstr(t, w)
		instr.incoming, instr.mocks = incoming, mocks
		cfg := instrConfig(instr.composeInstr, utils.Native, "go test -p 1 -json ./...")
		cfg.Path = dir
		withRequests(cfg)
		svc := New(logger, instr, mockdb.New(zap.NewNop(), dir, ""), mapdb.New(zap.NewNop(), dir, ""), nil, nil, cfg)
		svc.(TestDBSetter).SetTestDB(db)
		require.NoError(t, svc.Record(context.Background()))
	}
	const set = "set"
	ctx := context.Background()
	mdb := mapdb.New(zap.NewNop(), dir, "")
	callback := func(name string, at time.Duration) *models.TestCase {
		return httpCase(name, "POST", "http://localhost:8080/webhooks/pay?attempt=1", 200, "ok", runnerT0.Add(at))
	}

	record(zap.NewNop(), &memTestDB{}, []*models.TestCase{callback("post-webhooks-pay-1", 4*time.Millisecond)},
		httpMockAt("POST", chargeURL, runnerT0.Add(2*time.Millisecond)),
		httpMockAt("GET", chargeURL+"/ch_1", runnerT0.Add(6*time.Millisecond)))
	mp, err := mdb.GetMapping(ctx, set)
	require.NoError(t, err)
	require.Len(t, mp.TestCases, 1)
	require.Equal(t, []string{"mock-0", "mock-1"}, mp.TestCases[0].MockNames())
	// The edit an agent makes to accept the delivery.
	accepted := []models.Delivery{{Case: "post-webhooks-pay-1", After: "mock-0", Sign: paySign}}
	require.NoError(t, mdb.UpsertCases(ctx, set, map[string]models.MappedTestCase{paidTest: {At: runnerT0, Deliver: accepted}}, nil, nil))
	mp, err = mdb.GetMapping(ctx, set)
	require.NoError(t, err)
	require.Equal(t, accepted, mp.TestCases[0].Deliver, "the mapping keeps the accepted entry")
	require.NoError(t, mdb.UpsertCases(ctx, set, map[string]models.MappedTestCase{paidTest: {Cases: []string{"post-webhooks-pay-1"}}}, nil, nil))
	mp, err = mdb.GetMapping(ctx, set)
	require.NoError(t, err)
	require.Equal(t, accepted, mp.TestCases[0].Deliver, "a write of the test's cases alone keeps its entries; only a record's run of the test replaces them")

	// Recorded again: a lookup comes first now, so the charge is mock-1, and
	// the callback is case post-webhooks-pay-2.
	core, logs := observer.New(zap.InfoLevel)
	record(zap.New(core), &memTestDB{existing: []*models.TestCase{callback("post-webhooks-pay-1", 4*time.Millisecond)}},
		[]*models.TestCase{callback("post-webhooks-pay-2", 5*time.Millisecond)},
		httpMockAt("GET", "http://pay.example:18110/v1/customers/c_1", runnerT0.Add(1*time.Millisecond)),
		httpMockAt("POST", chargeURL, runnerT0.Add(3*time.Millisecond)),
		httpMockAt("GET", chargeURL+"/ch_1", runnerT0.Add(7*time.Millisecond)))
	mp, err = mdb.GetMapping(ctx, set)
	require.NoError(t, err)
	require.Len(t, mp.TestCases, 1)
	require.Equal(t, []string{"post-webhooks-pay-2"}, mp.TestCases[0].Cases)
	require.Equal(t, []models.Delivery{{Case: "post-webhooks-pay-2", After: "mock-1", Sign: paySign}}, mp.TestCases[0].Deliver,
		"the entry names the new recording's callback case and charge call, and keeps its sign:")
	said := logs.FilterMessage(`test "pay::TestPaidByCallback": the deliver: entry {case: post-webhooks-pay-1, after: mock-0} of the previous recording is carried as {case: post-webhooks-pay-2, after: mock-1}: the one POST /webhooks/pay case and the one POST pay.example:18110/v1/charges call of this recording of the test`).All()
	require.Len(t, said, 1, "the record says the carried entry with its old and new names")
	require.Equal(t, zap.InfoLevel, said[0].Level)
}

// An entry keploy cannot place in the new recording is dropped, never
// guessed: two cases or calls that both match, none, or a case another entry
// took; each is said at WARN with which entry, why and how to add it back. A
// test the recording did not run is left as it is.
func TestAReRecordDropsADeliverEntryItCannotPlace(t *testing.T) {
	entry := priorDelivery{Delivery: models.Delivery{Case: "post-webhooks-pay-1", After: "mock-0"}, caseCall: payCall, afterCall: "POST pay.example:18110/v1/charges"}
	cases := []capturedMock{{name: "post-webhooks-pay-2", call: payCall}, {name: "post-webhooks-pay-3", call: payCall}, {name: "get-orders-1", call: "GET /orders"}}
	mocks := []capturedMock{{name: "mock-1", call: "POST pay.example:18110/v1/charges"}, {name: "mock-2", call: "POST pay.example:18110/v1/charges"}, {name: "mock-3", call: "GET pay.example:18110/v1/charges/ch_1"}, {name: "mock-4"}}
	const drop = `test "pay::TestPaidByCallback": the deliver: entry {case: post-webhooks-pay-1, after: mock-0} of the previous recording is dropped: `
	const back = `; to deliver its callback again, add the entry back under the test's deliver: in mappings.yaml with the names its case and its after: call have in this recording`
	for _, tt := range []struct {
		name    string
		prior   []priorDelivery
		cases   []string
		mocks   []string
		deliver []models.Delivery
		lines   []carriedLine
	}{
		{"two cases match", []priorDelivery{entry}, []string{"get-orders-1", "post-webhooks-pay-2", "post-webhooks-pay-3"}, []string{"mock-1"}, nil,
			[]carriedLine{{msg: drop + "this recording of the test has 2 cases that are a POST /webhooks/pay, as its case was (post-webhooks-pay-2, post-webhooks-pay-3), and keploy never guesses which one it is" + back}}},
		{"no case matches", []priorDelivery{entry}, []string{"get-orders-1"}, []string{"mock-1"}, nil,
			[]carriedLine{{msg: drop + "this recording of the test has no case that is a POST /webhooks/pay, as its case was" + back}}},
		{"two calls match", []priorDelivery{entry}, []string{"post-webhooks-pay-2"}, []string{"mock-1", "mock-2", "mock-3"}, nil,
			[]carriedLine{{msg: drop + "this recording of the test has 2 HTTP calls that are a POST pay.example:18110/v1/charges, as its after: was (mock-1, mock-2), and keploy never guesses which one it is" + back}}},
		{"no call matches", []priorDelivery{entry}, []string{"post-webhooks-pay-2"}, []string{"mock-3", "mock-4"}, nil,
			[]carriedLine{{msg: drop + "this recording of the test has no HTTP call that is a POST pay.example:18110/v1/charges, as its after: was" + back}}},
		{"lost", []priorDelivery{{Delivery: entry.Delivery, lost: "its case post-webhooks-pay-1 is not a recorded HTTP request of the previous recording"}}, []string{"post-webhooks-pay-2"}, []string{"mock-1"}, nil,
			[]carriedLine{{msg: drop + "its case post-webhooks-pay-1 is not a recorded HTTP request of the previous recording" + back}}},
		{"a case taken", []priorDelivery{entry, {Delivery: models.Delivery{Case: "post-webhooks-pay-7", After: "mock-5"}, caseCall: payCall, afterCall: "GET pay.example:18110/v1/charges/ch_1"}},
			[]string{"post-webhooks-pay-2"}, []string{"mock-1", "mock-3"}, []models.Delivery{{Case: "post-webhooks-pay-2", After: "mock-1"}},
			[]carriedLine{
				{kept: true, msg: `test "pay::TestPaidByCallback": the deliver: entry {case: post-webhooks-pay-1, after: mock-0} of the previous recording is carried as {case: post-webhooks-pay-2, after: mock-1}: the one POST /webhooks/pay case and the one POST pay.example:18110/v1/charges call of this recording of the test`},
				{msg: `test "pay::TestPaidByCallback": the deliver: entry {case: post-webhooks-pay-7, after: mock-5} of the previous recording is dropped: its case in this recording, post-webhooks-pay-2, is another entry's case already` + back},
			}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var entries []models.MockEntry
			for _, n := range tt.mocks {
				entries = append(entries, models.MockEntry{Name: n, Kind: string(models.HTTP)})
			}
			byCase := map[string]models.MappedTestCase{paidTest: {Cases: tt.cases}, "pay::TestOther": {Cases: []string{"get-orders-1"}}}
			prior := map[string][]priorDelivery{paidTest: tt.prior, "pay::TestNotRun": {entry}}
			windows := []models.ScopeWindow{{Name: paidTest}, {Name: "pay::TestOther"}}
			require.Equal(t, tt.lines, carriedDeliveries(prior, windows, byCase, map[string][]models.MockEntry{paidTest: entries}, cases, mocks))
			require.Equal(t, tt.deliver, byCase[paidTest].Deliver)
			require.NotContains(t, byCase, "pay::TestNotRun", "a test the recording did not run is left as it is")
			require.Nil(t, byCase["pay::TestOther"].Deliver)
		})
	}
	require.Nil(t, carriedDeliveries(nil, []models.ScopeWindow{{Name: paidTest}}, map[string]models.MappedTestCase{}, nil, cases, mocks))

	// A test that ran with no case at all still gets its line.
	byCase := map[string]models.MappedTestCase{}
	require.Equal(t, []carriedLine{{msg: drop + "this recording of the test has no case that is a POST /webhooks/pay, as its case was" + back}},
		carriedDeliveries(map[string][]priorDelivery{paidTest: {entry}}, []models.ScopeWindow{{Name: paidTest}}, byCase, nil, nil, nil))

	// Carried at INFO, dropped at WARN.
	core, logs := observer.New(zap.InfoLevel)
	svc := &mockService{logger: zap.New(core)}
	svc.carryDeliveries("set", map[string][]priorDelivery{paidTest: {entry}, "pay::TestB": {entry}},
		[]models.ScopeWindow{{Name: paidTest}, {Name: "pay::TestB"}},
		map[string]models.MappedTestCase{paidTest: {Cases: []string{"post-webhooks-pay-2"}}, "pay::TestB": {}},
		map[string][]models.MockEntry{paidTest: {{Name: "mock-1", Kind: string(models.HTTP)}}}, cases, mocks)
	got := logs.All()
	require.Len(t, got, 2)
	require.Equal(t, zap.WarnLevel, got[0].Level)
	require.Contains(t, got[0].Message, `test "pay::TestB": the deliver: entry {case: post-webhooks-pay-1, after: mock-0} of the previous recording is dropped`)
	require.Equal(t, zap.InfoLevel, got[1].Level)
	require.Contains(t, got[1].Message, "is carried as {case: post-webhooks-pay-2, after: mock-1}")
}

// Before a re-record deletes the mappings, keploy reads each deliver: entry
// with the calls its case and after: stood for; an entry whose calls cannot
// be read carries why, and a mapping that cannot be read is said.
func TestPriorDeliveriesReadTheCallsTheEntriesStoodFor(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	mdb := mapdb.New(zap.NewNop(), dir, "")
	t.Setenv(provenswitch.Env, "1")
	require.NoError(t, mdb.UpsertBatch(ctx, "set", map[string][]models.MockEntry{
		paidTest:      {{Name: "mock-0", Kind: string(models.HTTP)}, {Name: "mock-1", Kind: string(models.HTTP)}, {Name: "mock-2", Kind: string(models.HTTP)}},
		"pay::TestNo": {{Name: "mock-3", Kind: string(models.HTTP)}},
	}))
	deliver := []models.Delivery{{Case: "post-webhooks-pay-1", After: "mock-0", Sign: paySign}, {Case: "test-2", After: "mock-1"}, {Case: "test-3", After: "mock-2"}}
	require.NoError(t, mdb.UpsertCases(ctx, "set", map[string]models.MappedTestCase{
		paidTest:      {At: runnerT0, Cases: []string{"post-webhooks-pay-1", "test-2", "test-3"}, Deliver: deliver},
		"pay::TestNo": {At: runnerT0, Cases: []string{"test-4"}},
	}, nil, nil))
	grpc := &models.TestCase{Name: "test-2", Kind: models.GRPC_EXPORT}
	cases := casesDB{cases: []*models.TestCase{nil, httpCase("post-webhooks-pay-1", "POST", "http://localhost:8080/webhooks/pay?x=1", 200, "ok", runnerT0), grpc, httpCase("test-4", "GET", "/orders", 200, "", runnerT0)}}
	pg := &models.Mock{Name: "mock-1", Kind: models.Postgres}
	mocks := walkMocks{mocks: []*models.Mock{httpMockAt("POST", chargeURL, runnerT0), pg, httpMockAt("GET", "/x", runnerT0)}}
	mocks.mocks[0].Name, mocks.mocks[2].Name = "mock-0", "mock-9"
	svc := func(testDB TestDB, mockDB MockDB) *mockService {
		return &mockService{logger: zap.NewNop(), mappingDB: mdb, testDB: testDB, mockDB: mockDB}
	}
	paid := priorDelivery{Delivery: deliver[0], caseCall: payCall, afterCall: "POST pay.example:18110/v1/charges"}

	require.Equal(t, map[string][]priorDelivery{paidTest: {
		paid,
		{Delivery: deliver[1], lost: "its case test-2 is not a recorded HTTP request of the previous recording"},
		{Delivery: deliver[2], lost: "its case test-3 is not a recorded HTTP request of the previous recording"},
	}}, svc(cases, mocks).priorDeliveries(ctx, "set"))

	cases.cases = append(cases.cases, httpCase("test-2", "PUT", "/a", 200, "", runnerT0), httpCase("test-3", "PUT", "/b", 200, "", runnerT0))
	cases.cases[2] = nil
	require.Equal(t, map[string][]priorDelivery{paidTest: {
		paid,
		{Delivery: deliver[1], caseCall: "PUT /a", lost: "its after: mock-1 is not a recorded HTTP call of the previous recording"},
		{Delivery: deliver[2], caseCall: "PUT /b", lost: "its after: mock-2 is not a recorded HTTP call of the previous recording"},
	}}, svc(cases, mocks).priorDeliveries(ctx, "set"), "a case read, its after: not an HTTP call, or not in the mocks")

	for _, tt := range []struct {
		name   string
		testDB TestDB
		mockDB MockDB
		lost   string
	}{
		{"no cases kept", nil, mocks, "this keploy keeps no recorded cases (mock.recordRequests is false in keploy.yml)"},
		{"cases unreadable", casesDB{err: errors.New("disk")}, mocks, "the previous recording's cases could not be read: disk"},
		{"mocks not walked", cases, stubMockDB{}, "this keploy's mock store cannot read the previous recording's mocks one by one"},
		{"mocks unreadable", cases, walkMocks{err: errors.New("cut file")}, "the previous recording's mocks could not be read: cut file"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := svc(tt.testDB, tt.mockDB).priorDeliveries(ctx, "set")
			require.Len(t, got[paidTest], 3)
			for _, d := range got[paidTest] {
				require.Equal(t, tt.lost, d.lost)
			}
		})
	}

	require.Nil(t, svc(cases, mocks).priorDeliveries(ctx, "none"), "a set with no mappings has nothing to carry")
	require.Nil(t, (&mockService{logger: zap.NewNop(), mockDB: mocks}).priorDeliveries(ctx, "set"), "no mapping store, nothing to carry")
	require.NoError(t, mdb.UpsertCases(ctx, "plain", map[string]models.MappedTestCase{"pay::TestNo": {At: runnerT0}}, nil, nil))
	require.Nil(t, svc(cases, mocks).priorDeliveries(ctx, "plain"), "a set with no deliver: entry has nothing to carry")

	// An unreadable mapping is said, and nothing is carried.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "set", "mappings.yaml"), []byte("tests: [\n"), 0o644))
	core, logs := observer.New(zap.InfoLevel)
	bad := svc(cases, mocks)
	bad.logger = zap.New(core)
	require.Nil(t, bad.priorDeliveries(ctx, "set"))
	said := logs.FilterMessage("keploy could not read the set's mappings.yaml before recording it again, so it carries none of its deliver: entries into this recording").All()
	require.Len(t, said, 1)
	require.Equal(t, zap.WarnLevel, said[0].Level)

	// Without the proven switch deliver: is never read, and nothing is said.
	t.Setenv(provenswitch.Env, "")
	core, logs = observer.New(zap.InfoLevel)
	bad.logger = zap.New(core)
	require.Nil(t, bad.priorDeliveries(ctx, "set"))
	require.Zero(t, logs.Len())
}

// The calls a carried entry is found by: a case by method and path, a mock
// by method, host and path; neither by query.
func TestDeliverCallsAreMethodAndPath(t *testing.T) {
	require.Equal(t, "POST /webhooks/pay", caseCall(httpCase("c", "POST", "http://localhost:8080/webhooks/pay?attempt=2", 200, "", runnerT0)))
	require.Equal(t, "", caseCall(&models.TestCase{Kind: models.GRPC_EXPORT}))
	require.Equal(t, "", caseCall(nil))
	require.Equal(t, "POST pay.example:18110/v1/charges", mockCall(httpMockAt("POST", chargeURL+"?expand=1", runnerT0)))
	require.Equal(t, "", mockCall(&models.Mock{Kind: models.HTTP}), "an HTTP mock with no request")
	require.Equal(t, "", mockCall(&models.Mock{Kind: models.Postgres, Spec: models.MockSpec{HTTPReq: &models.HTTPReq{Method: "POST"}}}))
	require.Equal(t, "", mockCall(nil))
	require.Equal(t, "%zz/a", callPath("%zz/a?b=1", true), "not a URL: the raw path, without its query")
	require.Equal(t, "/v1/charges", callPath(chargeURL, false))

	// The record keeps each case's call as it is stored.
	c := &caseCapture{}
	c.add(httpCase("post-webhooks-pay-2", "POST", "http://localhost:8080/webhooks/pay", 200, "", runnerT0))
	require.Equal(t, payCall, c.list()[0].call)
}
