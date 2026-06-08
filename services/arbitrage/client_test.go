package arbitrage

import (
	"context"
	"net/http"
	"testing"

	"github.com/jarcoal/httpmock"
)

func TestFetchPolymarketNormalizes(t *testing.T) {
	hc := &http.Client{}
	httpmock.ActivateNonDefault(hc)
	defer httpmock.DeactivateAndReset()

	page0 := `[
	  {"conditionId":"0xabc","question":"Will BTC top 100k?","slug":"btc-100k",
	   "outcomes":"[\"Yes\", \"No\"]","outcomePrices":"[\"0.61\", \"0.39\"]",
	   "liquidityNum":"12345.6","active":true,"closed":false,"endDate":"2026-12-31T00:00:00Z"},
	  {"conditionId":"0xdead","question":"closed one","slug":"x",
	   "outcomes":"[\"Yes\", \"No\"]","outcomePrices":"[\"0.5\", \"0.5\"]","active":false,"closed":true}
	]`
	httpmock.RegisterResponder("GET", `=~^https://gamma-api\.polymarket\.com/markets`,
		func(req *http.Request) (*http.Response, error) {
			if req.URL.Query().Get("offset") == "0" {
				return httpmock.NewStringResponse(200, page0), nil
			}
			return httpmock.NewStringResponse(200, `[]`), nil
		})

	c := NewClient()
	c.HTTP = hc
	got, err := c.FetchPolymarket(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 active market (closed filtered), got %d", len(got))
	}
	m := got[0]
	if m.Venue != VenuePolymarket || m.ID != "0xabc" {
		t.Fatalf("bad market identity: %+v", m)
	}
	if !approx(m.YesPrice, 0.61) || !approx(m.NoPrice, 0.39) {
		t.Fatalf("bad prices: yes=%v no=%v", m.YesPrice, m.NoPrice)
	}
	if !approx(m.Liquidity, 12345.6) {
		t.Fatalf("bad liquidity: %v", m.Liquidity)
	}
	if m.URL != "https://polymarket.com/event/btc-100k" {
		t.Fatalf("bad url: %s", m.URL)
	}
}

func TestFetchLimitlessNormalizes(t *testing.T) {
	hc := &http.Client{}
	httpmock.ActivateNonDefault(hc)
	defer httpmock.DeactivateAndReset()

	page1 := `[
	  {"id":42,"address":"0xfeed","conditionId":"0xcond","title":"Will BTC top 100k?",
	   "slug":"btc-100k","prices":[58.0,42.0],"liquidity":"9000","volume":"55000",
	   "status":"FUNDED","expired":false,"expirationTimestamp":1798675200000},
	  {"id":43,"address":"0x0","title":"expired one","prices":[50,50],"expired":true}
	]`
	httpmock.RegisterResponder("GET", `=~^https://api\.limitless\.exchange/markets/active`,
		func(req *http.Request) (*http.Response, error) {
			if req.URL.Query().Get("page") == "1" {
				return httpmock.NewStringResponse(200, page1), nil
			}
			return httpmock.NewStringResponse(200, `[]`), nil
		})

	c := NewClient()
	c.HTTP = hc
	got, err := c.FetchLimitless(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 active market (expired filtered), got %d", len(got))
	}
	m := got[0]
	if m.Venue != VenueLimitless || m.ID != "0xcond" {
		t.Fatalf("bad identity: %+v", m)
	}
	if !approx(m.YesPrice, 0.58) || !approx(m.NoPrice, 0.42) {
		t.Fatalf("percent->prob conversion wrong: yes=%v no=%v", m.YesPrice, m.NoPrice)
	}
	if m.ExpiresAt.IsZero() {
		t.Fatal("expected non-zero expiry from ms timestamp")
	}
}
