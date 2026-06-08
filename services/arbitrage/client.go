package arbitrage

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// httpDoer lets tests inject a mock transport.
type httpDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Client fetches public, read-only market data from both venues. No auth, no
// wallet, no order placement — by construction it cannot move funds.
type Client struct {
	HTTP            httpDoer
	PolymarketBase  string // default https://gamma-api.polymarket.com
	LimitlessBase   string // default https://api.limitless.exchange
	PerVenuePageMax int    // safety cap on pagination
}

// NewClient returns a Client with production defaults.
func NewClient() *Client {
	return &Client{
		HTTP:            &http.Client{Timeout: 20 * time.Second},
		PolymarketBase:  "https://gamma-api.polymarket.com",
		LimitlessBase:   "https://api.limitless.exchange",
		PerVenuePageMax: 20,
	}
}

func (c *Client) getJSON(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// ---- Polymarket (Gamma API) ----------------------------------------------

// polymarketMarket mirrors the subset of GET /markets we use. Gamma encodes
// outcomes/outcomePrices as JSON *strings* (e.g. "[\"0.61\", \"0.39\"]"), so we
// take them as strings and parse defensively.
type polymarketMarket struct {
	ConditionID    string `json:"conditionId"`
	Question       string `json:"question"`
	Slug           string `json:"slug"`
	Outcomes      string      `json:"outcomes"`
	OutcomePrices string      `json:"outcomePrices"`
	Liquidity     json.Number `json:"liquidityNum,omitempty"`
	LiquidityClob json.Number `json:"liquidity,omitempty"`
	Volume        json.Number `json:"volumeNum,omitempty"`
	Active        bool        `json:"active"`
	Closed        bool        `json:"closed"`
	EndDateISO    string      `json:"endDate"`
}

// FetchPolymarket returns active, open, binary YES/NO markets.
func (c *Client) FetchPolymarket(ctx context.Context) ([]Market, error) {
	var out []Market
	const limit = 100
	for page := 0; page < c.PerVenuePageMax; page++ {
		url := fmt.Sprintf("%s/markets?active=true&closed=false&limit=%d&offset=%d",
			c.PolymarketBase, limit, page*limit)
		var batch []polymarketMarket
		if err := c.getJSON(ctx, url, &batch); err != nil {
			return nil, fmt.Errorf("polymarket page %d: %w", page, err)
		}
		if len(batch) == 0 {
			break
		}
		for _, pm := range batch {
			if !pm.Active || pm.Closed {
				continue
			}
			m, ok := pm.normalize()
			if ok {
				out = append(out, m)
			}
		}
		if len(batch) < limit {
			break
		}
	}
	return out, nil
}

func (pm polymarketMarket) normalize() (Market, bool) {
	outcomes := parseJSONStringArray(pm.Outcomes)
	prices := parseJSONStringArray(pm.OutcomePrices)
	yes, no, ok := yesNoFromOutcomes(outcomes, prices)
	if !ok {
		return Market{}, false
	}
	liq := parseFloat(string(pm.Liquidity))
	if liq == 0 {
		liq = parseFloat(string(pm.LiquidityClob))
	}
	m := Market{
		Venue:     VenuePolymarket,
		ID:        pm.ConditionID,
		Title:     pm.Question,
		URL:       "https://polymarket.com/event/" + pm.Slug,
		YesPrice:  yes,
		NoPrice:   no,
		Liquidity: liq,
		Volume:    parseFloat(string(pm.Volume)),
		ExpiresAt: parseISOTime(pm.EndDateISO),
	}
	return m, true
}

// ---- Limitless -------------------------------------------------------------

// limitlessMarket mirrors the subset of GET /markets/active we use. `prices` is
// a numeric array [yesPercent, noPercent].
type limitlessMarket struct {
	ID                  json.Number `json:"id"`
	Address             string      `json:"address"`
	ConditionID         string      `json:"conditionId"`
	Title               string      `json:"title"`
	Slug                string      `json:"slug"`
	Prices              []float64   `json:"prices"`
	Liquidity           json.Number `json:"liquidity"`
	Volume              json.Number `json:"volume"`
	Status              string      `json:"status"`
	Expired             bool        `json:"expired"`
	ExpirationTimestamp int64       `json:"expirationTimestamp"`
}

// limitlessResponse covers both a bare array and a {data:[...]} envelope.
type limitlessResponse struct {
	Data    []limitlessMarket `json:"data"`
	Markets []limitlessMarket `json:"markets"`
}

func (c *Client) FetchLimitless(ctx context.Context) ([]Market, error) {
	var out []Market
	const limit = 100
	for page := 1; page <= c.PerVenuePageMax; page++ {
		url := fmt.Sprintf("%s/markets/active?page=%d&limit=%d", c.LimitlessBase, page, limit)
		batch, err := c.fetchLimitlessPage(ctx, url)
		if err != nil {
			return nil, fmt.Errorf("limitless page %d: %w", page, err)
		}
		if len(batch) == 0 {
			break
		}
		for _, lm := range batch {
			if lm.Expired {
				continue
			}
			m, ok := lm.normalize()
			if ok {
				out = append(out, m)
			}
		}
		if len(batch) < limit {
			break
		}
	}
	return out, nil
}

// fetchLimitlessPage tolerates either a bare-array or enveloped response shape.
func (c *Client) fetchLimitlessPage(ctx context.Context, url string) ([]limitlessMarket, error) {
	var bare []limitlessMarket
	if err := c.getJSON(ctx, url, &bare); err == nil && bare != nil {
		return bare, nil
	}
	var env limitlessResponse
	if err := c.getJSON(ctx, url, &env); err != nil {
		return nil, err
	}
	if len(env.Data) > 0 {
		return env.Data, nil
	}
	return env.Markets, nil
}

func (lm limitlessMarket) normalize() (Market, bool) {
	if len(lm.Prices) < 2 {
		return Market{}, false
	}
	yes := lm.Prices[0] / 100.0
	no := lm.Prices[1] / 100.0
	id := lm.ConditionID
	if id == "" {
		id = lm.Address
	}
	var exp time.Time
	if lm.ExpirationTimestamp > 0 {
		exp = time.UnixMilli(lm.ExpirationTimestamp)
	}
	m := Market{
		Venue:     VenueLimitless,
		ID:        id,
		Title:     lm.Title,
		URL:       "https://limitless.exchange/markets/" + lm.Slug,
		YesPrice:  yes,
		NoPrice:   no,
		Liquidity: parseFloat(string(lm.Liquidity)),
		Volume:    parseFloat(string(lm.Volume)),
		ExpiresAt: exp,
	}
	return m, true
}

// ---- parsing helpers -------------------------------------------------------

func parseJSONStringArray(s string) []string {
	if s == "" {
		return nil
	}
	var arr []string
	if err := json.Unmarshal([]byte(s), &arr); err != nil {
		return nil
	}
	return arr
}

// yesNoFromOutcomes maps parallel outcome/price arrays to YES/NO probabilities.
func yesNoFromOutcomes(outcomes, prices []string) (yes, no float64, ok bool) {
	if len(outcomes) != len(prices) || len(outcomes) != 2 {
		return 0, 0, false
	}
	for i, label := range outcomes {
		p := parseFloat(prices[i])
		switch strings.ToLower(strings.TrimSpace(label)) {
		case "yes":
			yes = p
		case "no":
			no = p
		}
	}
	if yes == 0 && no == 0 {
		return 0, 0, false
	}
	return yes, no, true
}

func parseFloat(s string) float64 {
	if s == "" {
		return 0
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return f
}

func parseISOTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}
