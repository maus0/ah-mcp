package tools

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	appie "github.com/gwillem/appie-go"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

const mockSlots = `{"data":{"orderDeliverySlots":[
 {"date":"2026-10-21","isFullyBooked":false,"slots":[
   {"startTime":"2026-10-21T18:00:00+02:00","endTime":"2026-10-21T20:00:00+02:00","isFullyBooked":false,
    "serviceCharge":{"defaultPrice":{"amount":5.95},"price":{"amount":3.95}},"nudgeType":"SUSTAINABLE"},
   {"startTime":"2026-10-21T08:00:00+02:00","endTime":"2026-10-21T10:00:00+02:00","isFullyBooked":true,
    "serviceCharge":{"defaultPrice":{"amount":5.95},"price":{"amount":5.95}},"nudgeType":null}]},
 {"date":"2026-10-20","isFullyBooked":false,"slots":[
   {"startTime":"17:00","endTime":"19:00","isFullyBooked":false,
    "serviceCharge":{"defaultPrice":{"amount":6.95},"price":{"amount":6.95}},"nudgeType":["EXCLUDE_INEFFICIENT"]}]}
]}}`

const mockMember = `{"data":{"member":{"id":1,"name":{"first":"Test","last":"User"},
 "address":{"street":"Teststraat","houseNumber":12,"houseNumberExtra":"","postalCode":"1234 AB","city":"Utrecht","countryCode":"NL"}}}}`

func TestFlattenSlotsSortsAndFilters(t *testing.T) {
	var resp deliverySlotsResponse
	if err := json.Unmarshal([]byte(mockSlots), &struct {
		Data *deliverySlotsResponse `json:"data"`
	}{&resp}); err != nil {
		t.Fatal(err)
	}

	all := flattenSlots(&resp, slotFilter{MaxPrice: -1})
	if len(all) != 3 {
		t.Fatalf("got %d slots, want 3", len(all))
	}
	if all[0].Date != "2026-10-20" || all[0].Start != "17:00" || all[0].Weekday != "dinsdag" {
		t.Fatalf("first slot = %+v, want dinsdag 2026-10-20 17:00", all[0])
	}
	if all[1].Start != "08:00" || all[1].Available {
		t.Fatalf("second slot = %+v, want the booked 08:00 slot", all[1])
	}
	if all[2].End != "20:00" || all[2].Price == nil || *all[2].Price != 3.95 || all[2].Nudge != "SUSTAINABLE" {
		t.Fatalf("third slot = %+v", all[2])
	}

	avail := flattenSlots(&resp, slotFilter{OnlyAvailable: true, MaxPrice: -1})
	if len(avail) != 2 {
		t.Fatalf("only_available gave %d, want 2", len(avail))
	}
	if got := flattenSlots(&resp, slotFilter{From: "2026-10-21", MaxPrice: -1}); len(got) != 2 {
		t.Fatalf("from_date gave %d, want 2", len(got))
	}
	if got := flattenSlots(&resp, slotFilter{To: "2026-10-20", MaxPrice: -1}); len(got) != 1 {
		t.Fatalf("to_date gave %d, want 1", len(got))
	}
	if got := flattenSlots(&resp, slotFilter{MaxPrice: 4}); len(got) != 1 {
		t.Fatalf("max_price gave %d, want 1", len(got))
	}
}

// The tool must only ever send GraphQL queries, never mutations.
func TestDeliverySlotsToolIsReadOnly(t *testing.T) {
	var slotCalls, failOnce atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/graphql" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		var gq struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.Unmarshal(body, &gq)
		if strings.Contains(strings.ToLower(gq.Query), "mutation") {
			t.Errorf("tool sent a mutation: %s", gq.Query)
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(gq.Query, "orderDeliverySlots"):
			if failOnce.Add(1) == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte("busy"))
				return
			}
			slotCalls.Add(1)
			addr, _ := gq.Variables["address"].(map[string]any)
			if addr["postalCode"] != "1234AB" || addr["houseNumber"] != float64(12) {
				t.Errorf("address variables = %v", addr)
			}
			if _, ok := addr["houseNumberExtra"]; ok {
				t.Errorf("empty houseNumberExtra should be omitted")
			}
			_, _ = w.Write([]byte(mockSlots))
		case strings.Contains(gq.Query, "FetchMember"):
			_, _ = w.Write([]byte(mockMember))
		default:
			t.Errorf("unexpected query: %s", gq.Query)
		}
	}))
	defer srv.Close()

	client := appie.New(appie.WithBaseURL(srv.URL), appie.WithTokens("test", ""))
	deps := Deps{
		IsAuthenticated: func() bool { return true },
		RefreshIfNeeded: func(context.Context) error { return nil },
		GetClient:       func() (*appie.Client, error) { return client, nil },
	}
	s := server.NewMCPServer("test", "0")
	RegisterDeliveryTools(s, deps)
	st := s.GetTool("ah_get_delivery_slots")
	if st == nil {
		t.Fatal("tool not registered")
	}
	if ro := st.Tool.Annotations.ReadOnlyHint; ro == nil || !*ro {
		t.Fatal("tool must carry readOnlyHint=true")
	}

	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{"from_date": "2026-10-21"}
	res, err := st.Handler(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	text := resultText(t, res)
	if res.IsError {
		t.Fatalf("tool error: %s", text)
	}
	var out struct {
		PostalArea string         `json:"postal_area"`
		Count      int            `json:"count"`
		Slots      []DeliverySlot `json:"slots"`
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("bad json %q: %v", text, err)
	}
	if out.Count != 1 || out.Slots[0].Start != "18:00" || out.PostalArea != "1234**" {
		t.Fatalf("output = %+v", out)
	}
	if slotCalls.Load() != 1 {
		t.Fatalf("slot query succeeded %d times, want 1 (after one 503 retry)", slotCalls.Load())
	}

	bad := mcp.CallToolRequest{}
	bad.Params.Arguments = map[string]any{"to_date": "21-10-2026"}
	if res, _ := st.Handler(context.Background(), bad); !res.IsError {
		t.Fatal("invalid to_date should be rejected")
	}
}
