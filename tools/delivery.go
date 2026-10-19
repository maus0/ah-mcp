package tools

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	appie "github.com/gwillem/appie-go"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// deliverySlotsQuery is the read-only query the Appie app uses to show the
// "kies een moment" grid. It takes only an address and returns days with
// their slots; it does not reserve, select or change anything.
const deliverySlotsQuery = `query FetchDeliveryOrderSlotDays($address: MemberAddressInput!) {
  orderDeliverySlots(address: $address) {
    date
    isFullyBooked
    slots {
      startTime
      endTime
      deliveryLocationId
      shiftCode
      isFullyBooked
      serviceCharge { defaultPrice { amount } price { amount } }
      nudgeType
    }
  }
}`

type deliverySlotsResponse struct {
	OrderDeliverySlots []struct {
		Date          string `json:"date"`
		IsFullyBooked bool   `json:"isFullyBooked"`
		Slots         []struct {
			StartTime     string `json:"startTime"`
			EndTime       string `json:"endTime"`
			IsFullyBooked bool   `json:"isFullyBooked"`
			ServiceCharge *struct {
				DefaultPrice *struct {
					Amount float64 `json:"amount"`
				} `json:"defaultPrice"`
				Price *struct {
					Amount float64 `json:"amount"`
				} `json:"price"`
			} `json:"serviceCharge"`
			NudgeType any `json:"nudgeType"`
		} `json:"slots"`
	} `json:"orderDeliverySlots"`
}

// DeliverySlot is one delivery window as returned by ah_get_delivery_slots.
type DeliverySlot struct {
	Date         string   `json:"date"`
	Weekday      string   `json:"weekday,omitempty"`
	Start        string   `json:"start"`
	End          string   `json:"end"`
	Price        *float64 `json:"price_eur,omitempty"`
	DefaultPrice *float64 `json:"default_price_eur,omitempty"`
	Available    bool     `json:"available"`
	Nudge        string   `json:"nudge,omitempty"`
}

// fetchDeliverySlots runs the read-only slot query for an address.
func fetchDeliverySlots(ctx context.Context, c *appie.Client, addr appie.Address) (*deliverySlotsResponse, error) {
	address := map[string]any{
		"street":      addr.Street,
		"houseNumber": addr.HouseNumber,
		"postalCode":  strings.ReplaceAll(addr.PostalCode, " ", ""),
		"city":        addr.City,
		"countryCode": addr.CountryCode,
	}
	if address["countryCode"] == "" {
		address["countryCode"] = "NL"
	}
	withExtra := addr.HouseNumberExtra != ""
	if withExtra {
		address["houseNumberExtra"] = addr.HouseNumberExtra
	}

	var resp deliverySlotsResponse
	err := c.DoGraphQL(ctx, deliverySlotsQuery, map[string]any{"address": address}, &resp)
	if err != nil && withExtra && strings.Contains(err.Error(), "houseNumberExtra") {
		// The field set of MemberAddressInput is not published; if AH
		// rejects the extension, retry with the five verified fields.
		delete(address, "houseNumberExtra")
		resp = deliverySlotsResponse{}
		err = c.DoGraphQL(ctx, deliverySlotsQuery, map[string]any{"address": address}, &resp)
	}
	if err != nil {
		return nil, err
	}
	return &resp, nil
}

var dutchWeekdays = [...]string{"zondag", "maandag", "dinsdag", "woensdag", "donderdag", "vrijdag", "zaterdag"}

// slotDate normalises a day date ("2026-10-20" or an RFC3339 timestamp) to
// YYYY-MM-DD in Europe/Amsterdam.
func slotDate(s string) (string, time.Time, bool) {
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return s, t, true
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		t = t.In(amsterdam())
		return t.Format("2006-01-02"), t, true
	}
	if len(s) >= 10 {
		if t, err := time.Parse("2006-01-02", s[:10]); err == nil {
			return s[:10], t, true
		}
	}
	return s, time.Time{}, false
}

// slotClock turns "18:00", "18:00:00" or an RFC3339 timestamp into "18:00".
func slotClock(s string) string {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.In(amsterdam()).Format("15:04")
	}
	if i := strings.IndexByte(s, 'T'); i >= 0 {
		s = s[i+1:]
	}
	if len(s) >= 5 && s[2] == ':' {
		return s[:5]
	}
	return s
}

func amsterdam() *time.Location {
	if loc, err := time.LoadLocation("Europe/Amsterdam"); err == nil {
		return loc
	}
	return time.FixedZone("CET", 3600)
}

func nudgeString(v any) string {
	switch n := v.(type) {
	case nil:
		return ""
	case string:
		return n
	case []any:
		parts := make([]string, 0, len(n))
		for _, p := range n {
			if s, ok := p.(string); ok && s != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, ",")
	default:
		return fmt.Sprint(n)
	}
}

// slotFilter holds the optional, client-side filters of the tool.
type slotFilter struct {
	From, To      string // YYYY-MM-DD, inclusive; empty means unbounded
	OnlyAvailable bool
	MaxPrice      float64 // < 0 means no limit
}

// flattenSlots converts the API response into a sorted, filtered list.
func flattenSlots(resp *deliverySlotsResponse, f slotFilter) []DeliverySlot {
	out := []DeliverySlot{}
	for _, day := range resp.OrderDeliverySlots {
		date, t, ok := slotDate(day.Date)
		if f.From != "" && date < f.From {
			continue
		}
		if f.To != "" && date > f.To {
			continue
		}
		weekday := ""
		if ok {
			weekday = dutchWeekdays[t.Weekday()]
		}
		for _, s := range day.Slots {
			available := !s.IsFullyBooked && !day.IsFullyBooked
			if f.OnlyAvailable && !available {
				continue
			}
			ds := DeliverySlot{
				Date:      date,
				Weekday:   weekday,
				Start:     slotClock(s.StartTime),
				End:       slotClock(s.EndTime),
				Available: available,
				Nudge:     nudgeString(s.NudgeType),
			}
			if sc := s.ServiceCharge; sc != nil {
				if sc.Price != nil {
					p := sc.Price.Amount
					ds.Price = &p
				}
				if sc.DefaultPrice != nil {
					p := sc.DefaultPrice.Amount
					ds.DefaultPrice = &p
				}
			}
			if f.MaxPrice >= 0 && ds.Price != nil && *ds.Price > f.MaxPrice {
				continue
			}
			out = append(out, ds)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Date != out[j].Date {
			return out[i].Date < out[j].Date
		}
		return out[i].Start < out[j].Start
	})
	return out
}

// isTransient reports errors worth retrying for a pure read: rate limits and
// AH's short-lived 502/503/504 responses. Safe only because nothing is written.
func isTransient(err error) bool {
	if isRateLimited(err) {
		return true
	}
	s := err.Error()
	return strings.Contains(s, " 502") || strings.Contains(s, " 503") || strings.Contains(s, " 504")
}

// withReadRetry retries a read-only call on transient errors (1s, 2s backoff).
func withReadRetry(ctx context.Context, tool string, fn func() error) error {
	var lastErr error
	for attempt := 0; attempt < retryMaxAttempts; attempt++ {
		if lastErr = fn(); lastErr == nil || !isTransient(lastErr) {
			return lastErr
		}
		if attempt == retryMaxAttempts-1 {
			break
		}
		wait := retryBaseDelay * (1 << uint(attempt))
		LogWarn(tool, "transient AH error, retrying in %v (attempt %d/%d)", wait, attempt+1, retryMaxAttempts)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
	return lastErr
}

func validDate(s string) bool {
	_, err := time.Parse("2006-01-02", s)
	return err == nil
}

// RegisterDeliveryTools registers the read-only delivery slot tool.
func RegisterDeliveryTools(s *server.MCPServer, deps Deps) {
	const name = "ah_get_delivery_slots"
	tool := mcp.NewTool(name,
		mcp.WithTitleAnnotation("Albert Heijn: Delivery Slots (read-only)"),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithDescription(
			"List Albert Heijn home-delivery time slots (bezorgmomenten) for the delivery address "+
				"on the member profile. READ-ONLY: does not reserve or select a slot and does not "+
				"create or change an order. Returns date, weekday, start, end, price_eur "+
				"(delivery cost), default_price_eur, available and nudge (e.g. sustainable slot). "+
				"Optional client-side filters: from_date/to_date (YYYY-MM-DD, inclusive), "+
				"only_available (default true), max_price.",
		),
		mcp.WithString("from_date", mcp.Description("Only slots on or after this date (YYYY-MM-DD)")),
		mcp.WithString("to_date", mcp.Description("Only slots on or before this date (YYYY-MM-DD)")),
		mcp.WithBoolean("only_available", mcp.Description("Hide fully booked slots (default true)")),
		mcp.WithNumber("max_price", mcp.Description("Only slots whose delivery cost in EUR is at most this amount")),
	)
	s.AddTool(tool, withClient(deps, func(ctx context.Context, c *appie.Client, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		f := slotFilter{
			From:          strings.TrimSpace(req.GetString("from_date", "")),
			To:            strings.TrimSpace(req.GetString("to_date", "")),
			OnlyAvailable: req.GetBool("only_available", true),
			MaxPrice:      req.GetFloat("max_price", -1),
		}
		if f.From != "" && !validDate(f.From) {
			return errResult("from_date must be YYYY-MM-DD"), nil
		}
		if f.To != "" && !validDate(f.To) {
			return errResult("to_date must be YYYY-MM-DD"), nil
		}

		var member *appie.Member
		if err := withReadRetry(ctx, name, func() error {
			var e error
			member, e = c.GetMember(ctx)
			return e
		}); err != nil {
			LogError(name, "get member failed: %v", err)
			return errResult(fmt.Sprintf("Failed to read delivery address from member profile: %v", err)), nil
		}
		addr := member.Address
		if addr.PostalCode == "" || addr.HouseNumber == 0 {
			return errResult("No delivery address on the AH member profile."), nil
		}

		var resp *deliverySlotsResponse
		if err := withReadRetry(ctx, name, func() error {
			var e error
			resp, e = fetchDeliverySlots(ctx, c, addr)
			return e
		}); err != nil {
			LogError(name, "failed: %v", err)
			return errResult(fmt.Sprintf("Failed to get delivery slots: %v", err)), nil
		}

		slots := flattenSlots(resp, f)
		pc := strings.ReplaceAll(addr.PostalCode, " ", "")
		if len(pc) > 4 {
			pc = pc[:4] + "**" // only the area is shown, enough to confirm the address
		}
		return jsonResult(struct {
			PostalArea string         `json:"postal_area"`
			Count      int            `json:"count"`
			Slots      []DeliverySlot `json:"slots"`
		}{pc, len(slots), slots})
	}))
}
