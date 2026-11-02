package mock

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// holding names every case whose span (its request to its response) holds
// at: the cases in flight when a call was made.
func holding(spans []models.ScopeWindow, at time.Time) []string {
	var names []string
	for _, w := range spans {
		if !at.Before(w.Start) && !at.After(w.End) {
			names = append(names, w.Name)
		}
	}
	return names
}

// sharedCharges says, for each call that correlateCases charged to more than
// one case, the call and the cases, a line a call in mocks' order: it was
// made while all of them were in flight, and the time alone cannot tell
// which one's handler made it.
func sharedCharges(byCase map[string]models.MappedTestCase, mocks []capturedMock) []string {
	cases := map[string][]string{}
	for _, tc := range byCase {
		for c, names := range tc.CaseMocks {
			for _, n := range names {
				cases[n] = append(cases[n], c)
			}
		}
	}
	var out []string
	for _, mk := range mocks {
		held := cases[mk.name]
		if len(held) < 2 {
			continue
		}
		slices.Sort(held)
		what := mk.call
		if what == "" {
			what = mk.kind
		}
		out = append(out, fmt.Sprintf("call %s (%s) is charged to each of the cases %s in case_mocks: it was made while all of them were in flight, and the time alone cannot tell which one's handler made it",
			mk.name, what, strings.Join(held, ", ")))
	}
	return out
}

// saySharedCharges logs sharedCharges, a line a call.
func (m *mockService) saySharedCharges(name string, byCase map[string]models.MappedTestCase, mocks []capturedMock) {
	for _, l := range sharedCharges(byCase, mocks) {
		m.logger.Info(l, zap.String("mock-set", name))
	}
}
