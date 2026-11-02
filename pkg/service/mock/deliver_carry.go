package mock

import (
	"context"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"

	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// priorDelivery is a deliver: entry of the set's previous recording
// (models.Delivery) with the calls its names stood for there: its case's
// request (caseCall) and its after: mock's (afterCall). A re-record names
// its cases and mocks anew, so carryDeliveries finds the entry's case and
// mock again by these. lost says why it cannot ("" when it can).
type priorDelivery struct {
	models.Delivery
	caseCall  string
	afterCall string
	lost      string
}

// caseCall is the recorded HTTP request a deliver: entry's case stands for,
// as a re-record finds it again: its method and path ("POST /webhooks/pay").
// "" for a case that is not an HTTP request.
func caseCall(tc *models.TestCase) string {
	if tc == nil || tc.Kind != models.HTTP {
		return ""
	}
	return string(tc.HTTPReq.Method) + " " + callPath(tc.HTTPReq.URL, false)
}

// mockCall is the HTTP call a deliver: entry's after: mock stands for, as a
// re-record finds it again: its method, host and path ("POST
// pay.example:443/v1/charges"). "" for a mock that is not an HTTP call.
func mockCall(mk *models.Mock) string {
	if mk == nil || mk.Kind != models.HTTP || mk.Spec.HTTPReq == nil {
		return ""
	}
	return string(mk.Spec.HTTPReq.Method) + " " + callPath(mk.Spec.HTTPReq.URL, true)
}

// callPath is raw's path, after its host when withHost; raw without its
// query when it is not a URL.
func callPath(raw string, withHost bool) string {
	u, err := url.Parse(raw)
	if err != nil {
		path, _, _ := strings.Cut(raw, "?")
		return path
	}
	if withHost {
		return u.Host + u.Path
	}
	return u.Path
}

// priorDeliveries reads, by test, the deliver: entries of the set's previous
// recording, with the calls their names stood for there, before a re-record
// deletes its mappings: carryDeliveries keeps each in the new recording when
// it finds its calls there. Nil without the proven switch (deliver: is
// read only under it), when the set has none, or when its mappings cannot be
// read, which is said.
func (m *mockService) priorDeliveries(ctx context.Context, name string) map[string][]priorDelivery {
	if !provenswitch.On() {
		return nil
	}
	reader, ok := m.mappingDB.(MappingReader)
	if !ok {
		return nil
	}
	mp, err := reader.GetMapping(ctx, name)
	if err != nil {
		m.logger.Warn("keploy could not read the set's mappings.yaml before recording it again, so it carries none of its deliver: entries into this recording",
			zap.String("mock-set", name), zap.Error(err),
			zap.String("next_step", "add each test's deliver: entries back in mappings.yaml, with the names its callback case and trigger call have in this recording"))
		return nil
	}
	if mp == nil {
		return nil
	}
	byTest := make(map[string][]priorDelivery)
	mocks := map[string]bool{}
	for _, tc := range mp.TestCases {
		for _, d := range tc.Deliver {
			byTest[tc.ID] = append(byTest[tc.ID], priorDelivery{Delivery: d})
			mocks[d.After] = true
		}
	}
	if len(byTest) == 0 {
		return nil
	}
	caseCalls, casesLost := m.priorCaseCalls(ctx, name)
	mockCalls, mocksLost := m.priorMockCalls(ctx, name, mocks)
	for _, ds := range byTest {
		for i := range ds {
			d := &ds[i]
			d.caseCall, d.afterCall = caseCalls[d.Case], mockCalls[d.After]
			switch {
			case casesLost != "":
				d.lost = casesLost
			case d.caseCall == "":
				d.lost = fmt.Sprintf("its case %s is not a recorded HTTP request of the previous recording", d.Case)
			case mocksLost != "":
				d.lost = mocksLost
			case d.afterCall == "":
				d.lost = fmt.Sprintf("its after: %s is not a recorded HTTP call of the previous recording", d.After)
			}
		}
	}
	return byTest
}

// priorCaseCalls is, by name, each case of the set's previous recording's
// call (caseCall); lost says why none could be read.
func (m *mockService) priorCaseCalls(ctx context.Context, name string) (map[string]string, string) {
	if m.testDB == nil {
		return nil, "this keploy keeps no recorded cases (mock.recordRequests is false in keploy.yml)"
	}
	tcs, err := m.testDB.GetTestCases(ctx, name)
	if err != nil {
		return nil, fmt.Sprintf("the previous recording's cases could not be read: %v", err)
	}
	calls := make(map[string]string, len(tcs))
	for _, tc := range tcs {
		if tc != nil {
			calls[tc.Name] = caseCall(tc)
		}
	}
	return calls, ""
}

// priorMockCalls is, for each of the named mocks of the set's previous
// recording, its call (mockCall), read one mock at a time and kept for the
// named ones only: a set's mocks are never held whole. lost says why none
// could be read.
func (m *mockService) priorMockCalls(ctx context.Context, name string, want map[string]bool) (map[string]string, string) {
	walker, ok := m.mockDB.(pkg.TestSetMockWalker)
	if !ok {
		return nil, "this keploy's mock store cannot read the previous recording's mocks one by one"
	}
	calls := make(map[string]string, len(want))
	err := walker.WalkTestSetMocks(ctx, name, func(mk *models.Mock, _, _ bool) {
		if want[mk.Name] {
			calls[mk.Name] = mockCall(mk)
		}
	})
	if err != nil {
		return nil, fmt.Sprintf("the previous recording's mocks could not be read: %v", err)
	}
	return calls, ""
}

// carriedLine is what keploy record says of one deliver: entry of the
// previous recording: carried into the new one (kept), or dropped.
type carriedLine struct {
	kept bool
	msg  string
}

// carryDeliveries carries the deliver: entries of the set's previous
// recording into byCase (carriedDeliveries) and says each one: an entry
// carried at INFO, one dropped at WARN.
func (m *mockService) carryDeliveries(name string, prior map[string][]priorDelivery, windows []models.ScopeWindow, byCase map[string]models.MappedTestCase, byTest map[string][]models.MockEntry, cases, mocks []capturedMock) {
	for _, l := range carriedDeliveries(prior, windows, byCase, byTest, cases, mocks) {
		if l.kept {
			m.logger.Info(l.msg, zap.String("mock-set", name))
		} else {
			m.logger.Warn(l.msg, zap.String("mock-set", name))
		}
	}
}

// carriedDeliveries keeps each deliver: entry of the set's previous recording
// (prior) in the new recording of its test, if this recording ran the test
// (windows): its case is the one case of the test (byCase) whose request has
// the method and path its case's had, and its after: the one HTTP call of
// the test (byTest) to the method, host and path its after:'s went to (cases
// and mocks: the new recording's calls by name). An entry with no such case
// or call, or more than one, is dropped: keploy never guesses which one is
// the callback. A test this recording did not run is left as it is. Each
// entry is said: kept with its old and new names, or dropped with why and how
// to add it back.
func carriedDeliveries(prior map[string][]priorDelivery, windows []models.ScopeWindow, byCase map[string]models.MappedTestCase, byTest map[string][]models.MockEntry, cases, mocks []capturedMock) []carriedLine {
	if len(prior) == 0 {
		return nil
	}
	ran := make(map[string]bool, len(windows))
	for _, w := range windows {
		ran[w.Name] = true
	}
	// A case's and a mock's names never meet: a case is named for its
	// request ("post-webhooks-pay-2"), a mock "mock-<n>". Only HTTP calls
	// have a call (caseCall, mockCall), so only they match one.
	callOf := make(map[string]string, len(cases)+len(mocks))
	for _, c := range cases {
		callOf[c.name] = c.call
	}
	for _, mk := range mocks {
		callOf[mk.name] = mk.call
	}
	var lines []carriedLine
	for _, test := range slices.Sorted(maps.Keys(prior)) {
		if !ran[test] {
			continue
		}
		tc := byCase[test]
		var calls []string
		for _, e := range byTest[test] {
			calls = append(calls, e.Name)
		}
		taken := make(map[string]bool)
		for _, d := range prior[test] {
			was := fmt.Sprintf("{case: %s, after: %s}", d.Case, d.After)
			drop := func(why string) {
				lines = append(lines, carriedLine{msg: fmt.Sprintf("test %q: the deliver: entry %s of the previous recording is dropped: %s; to deliver its callback again, add the entry back under the test's deliver: in mappings.yaml with the names its case and its after: call have in this recording", test, was, why)})
			}
			if d.lost != "" {
				drop(d.lost)
				continue
			}
			c := matching(tc.Cases, callOf, d.caseCall)
			a := matching(calls, callOf, d.afterCall)
			switch {
			case len(c) != 1:
				drop(found(c, "case", d.caseCall, "case"))
			case len(a) != 1:
				drop(found(a, "HTTP call", d.afterCall, "after:"))
			case taken[c[0]]:
				drop(fmt.Sprintf("its case in this recording, %s, is another entry's case already", c[0]))
			default:
				taken[c[0]] = true
				tc.Deliver = append(tc.Deliver, models.Delivery{Case: c[0], After: a[0], Sign: d.Sign})
				lines = append(lines, carriedLine{kept: true, msg: fmt.Sprintf("test %q: the deliver: entry %s of the previous recording is carried as {case: %s, after: %s}: the one %s case and the one %s call of this recording of the test", test, was, c[0], a[0], d.caseCall, d.afterCall)})
			}
		}
		byCase[test] = tc
	}
	return lines
}

// matching is the names whose call (callOf) is call, in names' order.
func matching(names []string, callOf map[string]string, call string) []string {
	var out []string
	for _, n := range names {
		if callOf[n] == call {
			out = append(out, n)
		}
	}
	return out
}

// found says why names, the what of a test's new recording that are a call,
// as its entry's field was, carry no entry: there is none, or more than one.
func found(names []string, what, call, field string) string {
	if len(names) == 0 {
		return fmt.Sprintf("this recording of the test has no %s that is a %s, as its %s was", what, call, field)
	}
	return fmt.Sprintf("this recording of the test has %d %ss that are a %s, as its %s was (%s), and keploy never guesses which one it is", len(names), what, call, field, strings.Join(names, ", "))
}
