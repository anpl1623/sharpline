package theoddsapi

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// Schema conformance against the provider's own OpenAPI document.
//
// wire.go promises this by name: "The authoritative schema is
// testdata/docsamples/openapi_v4.json […] wire_test.go reads that file and
// asserts every json tag here names a property the provider actually documents,
// so a typo fails the build instead of silently decoding to a zero value."
//
// That failure mode is the reason this test exists rather than a review
// checklist. A struct tag of `json:"last_updated"` against a payload field
// called `last_update` does not error — it decodes to the zero time, the
// observation instant becomes absent, every price on the market is dropped for
// DropReasonNoObservationInstant, and the board just quietly empties.

// loadOpenAPI reads the document into a generic tree.
//
// Deliberately NOT into typed structs. The point of this file is to check what
// the provider documents against what this package assumes, and a typed reader
// would silently drop anything the types did not anticipate — which is the
// exact class of miss it exists to catch.
func loadOpenAPI(t *testing.T) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(readGolden(t, goldenOpenAPI), &doc); err != nil {
		t.Fatalf("parse %s: %v", goldenOpenAPI, err)
	}
	return doc
}

// documentedProperties collects every property name the document declares,
// anywhere. A flat set is the right granularity: the document nests schemas
// through $ref, and resolving those would test the resolver rather than the
// tags.
func documentedProperties(t *testing.T, doc map[string]any) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	var walk func(any)
	walk = func(v any) {
		switch n := v.(type) {
		case map[string]any:
			if props, ok := n["properties"].(map[string]any); ok {
				for k := range props {
					out[k] = true
				}
			}
			for _, child := range n {
				walk(child)
			}
		case []any:
			for _, child := range n {
				walk(child)
			}
		}
	}
	walk(doc)
	if len(out) == 0 {
		t.Fatalf("%s declares no properties at all; the assertion below would pass vacuously", goldenOpenAPI)
	}
	return out
}

func TestWireTagsNamePropertiesTheProviderDocuments(t *testing.T) {
	documented := documentedProperties(t, loadOpenAPI(t))

	types := []any{Sport{}, EventOdds{}, Bookmaker{}, Market{}, Outcome{}}
	for _, v := range types {
		rt := reflect.TypeOf(v)
		t.Run(rt.Name(), func(t *testing.T) {
			for i := 0; i < rt.NumField(); i++ {
				f := rt.Field(i)
				tag := strings.Split(f.Tag.Get("json"), ",")[0]
				if tag == "" || tag == "-" {
					t.Errorf("%s.%s has no json tag; the provider's field names do not match Go's",
						rt.Name(), f.Name)
					continue
				}
				if !documented[tag] {
					t.Errorf("%s.%s decodes %q, which %s does not document. A tag that names no real "+
						"property decodes to the zero value SILENTLY — it is not an error, it is an "+
						"empty board. Documented properties: %v",
						rt.Name(), f.Name, tag, goldenOpenAPI, sortedKeys(documented))
				}
			}
		})
	}
}

// TestQuotaHeaderNamesAreDocumented pins the three accounting headers.
//
// ADR 0003 requirement 3 makes the quota gauge depend on x-requests-remaining
// specifically. A rename would not fail anything at runtime: the header lookup
// would miss, sharpline_provider_quota_header_missing_total would climb, and
// the gauge would quietly revert to the local estimate that ADR 0003 says must
// not be the source of truth.
func TestQuotaHeaderNamesAreDocumented(t *testing.T) {
	doc := loadOpenAPI(t)
	declared := map[string]bool{}

	paths, ok := doc["paths"].(map[string]any)
	if !ok {
		t.Fatalf("%s has no paths object", goldenOpenAPI)
	}
	statuses := map[string]bool{}
	for _, ops := range paths {
		opMap, ok := ops.(map[string]any)
		if !ok {
			continue
		}
		for _, op := range opMap {
			opObj, ok := op.(map[string]any)
			if !ok {
				continue
			}
			responses, ok := opObj["responses"].(map[string]any)
			if !ok {
				continue
			}
			for code, resp := range responses {
				statuses[code] = true
				respObj, ok := resp.(map[string]any)
				if !ok {
					continue
				}
				headers, ok := respObj["headers"].(map[string]any)
				if !ok {
					continue
				}
				for h := range headers {
					declared[strings.ToLower(h)] = true
				}
			}
		}
	}

	for _, h := range []string{HeaderRequestsRemaining, HeaderRequestsUsed, HeaderRequestsLast} {
		if !declared[strings.ToLower(h)] {
			t.Errorf("%s does not declare the %q response header; the quota gauge reads it and would "+
				"silently fall back to a local estimate", goldenOpenAPI, h)
		}
	}

	// Every status this package classifies distinctly must be one the provider
	// documents. A status in classifyStatus that the document does not carry is
	// a guess; a documented status this package does not handle would fall
	// through to the retryable default and be retried for ever.
	for _, code := range []string{"401", "404", "422", "429", "500"} {
		if !statuses[code] {
			t.Errorf("%s does not document a %s response, but classifyStatus treats it as a distinct "+
				"failure mode", goldenOpenAPI, code)
		}
	}
}

// TestEndpointTemplatesMatchDocumentedPaths pins the metric label and span name
// vocabulary to the provider's own path templates.
//
// The templates are BOUNDED strings used as `endpoint` label values. A drift
// between a template and the concrete path this package builds would not fail
// anything — the request would still be made — but the dashboard's per-endpoint
// series would split in two.
func TestEndpointTemplatesMatchDocumentedPaths(t *testing.T) {
	doc := loadOpenAPI(t)
	paths, ok := doc["paths"].(map[string]any)
	if !ok {
		t.Fatalf("%s has no paths object", goldenOpenAPI)
	}
	for _, tmpl := range []string{EndpointSports, EndpointOdds, EndpointEventOdds} {
		if _, ok := paths[tmpl]; !ok {
			t.Errorf("%s is not a path %s documents; documented paths: %v",
				tmpl, goldenOpenAPI, sortedKeys(toBoolSet(paths)))
		}
	}
}

// TestSweepCostMatchesTheDocumentedBillingModel is the arithmetic ADR 0003
// depends on, stated as a table.
//
// Every scenario in that ADR is computed from `markets × regions` and from "a
// group of 10 bookmakers counts as one region-equivalent". An error here does
// not surface as a failure: it surfaces as the monthly budget running out
// earlier than the arithmetic said it would.
func TestSweepCostMatchesTheDocumentedBillingModel(t *testing.T) {
	cases := []struct {
		name                         string
		markets, regions, bookmakers int
		want                         int
	}{
		{"ADR 0003 recommended sweep: 3 markets, 1 region", 3, 1, 0, 3},
		{"two regions doubles it", 3, 2, 0, 6},
		{"one market, one region", 1, 1, 0, 1},
		{"markets omitted defaults to h2h and is still billed", 0, 1, 0, 1},
		{"regions omitted still bills one region-equivalent", 3, 0, 0, 3},
		{"ten named bookmakers are one region-equivalent", 3, 3, 10, 3},
		{"eleven bookmakers are two", 3, 3, 11, 6},
		{"twenty bookmakers are two", 1, 1, 20, 2},
		{"twenty-one bookmakers are three", 1, 1, 21, 3},
		{"bookmakers take precedence over regions", 2, 7, 5, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SweepCost(tc.markets, tc.regions, tc.bookmakers); got != tc.want {
				t.Errorf("SweepCost(%d, %d, %d) = %d, want %d",
					tc.markets, tc.regions, tc.bookmakers, got, tc.want)
			}
		})
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func toBoolSet(m map[string]any) map[string]bool {
	out := make(map[string]bool, len(m))
	for k := range m {
		out[k] = true
	}
	return out
}
