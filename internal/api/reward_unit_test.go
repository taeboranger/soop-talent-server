package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sooptalent/server/internal/auth"
	"github.com/sooptalent/server/internal/scoring"
	"github.com/sooptalent/server/internal/shop"
	"github.com/sooptalent/server/internal/store"
)

func TestRewardUnitAPI(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL 미설정")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, dsn, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	s := &Server{Store: st, Log: slog.Default(), Streamer: &fakeStreamer{}}
	handler := s.Routes()
	create := func(suffix string, approved bool) (string, string) {
		bid := fmt.Sprintf("unit_api_%d_%s", time.Now().UnixNano(), suffix)
		token, account, e := auth.LoginWithVerifiedSOOP(ctx, st.Pool, bid, "달란트닉네임", "")
		if e != nil {
			t.Fatal(e)
		}
		if approved {
			if e := st.EnsureStreamer(ctx, bid, ""); e != nil {
				t.Fatal(e)
			}
			if _, e := st.Pool.Exec(ctx, `INSERT INTO streamer_claims (account_id,bid,status) VALUES ($1,$2,'approved')`, account.ID, bid); e != nil {
				t.Fatal(e)
			}
		}
		t.Cleanup(func() {
			st.Pool.Exec(ctx, `DELETE FROM streamer_reward_units WHERE bid=$1`, bid)
			st.Pool.Exec(ctx, `DELETE FROM streamers WHERE bid=$1`, bid)
			st.Pool.Exec(ctx, `DELETE FROM accounts WHERE id=$1`, account.ID)
		})
		return token, bid
	}
	a, bidA := create("a", true)
	b, bidB := create("b", true)
	viewer, viewerID := create("viewer", false)
	request := func(token, method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, req)
		return out
	}
	for _, tc := range []struct {
		token, body string
		status      int
	}{
		{"", `{"unit":"달러"}`, 401}, {viewer, `{"unit":"달러"}`, 403}, {a, `{"unit":"<script>"}`, 400},
		{a, `{"unit":"달러","bid":"` + bidB + `"}`, 200}, {a, `{"unit":"달러"}`, 200}, {a, `{"unit":"코인"}`, 409}, {b, `{"unit":"코인"}`, 200},
	} {
		res := request(tc.token, http.MethodPut, "/api/streamer/reward-unit", tc.body)
		if res.Code != tc.status {
			t.Fatalf("PUT %s: status=%d want=%d body=%s", tc.body, res.Code, tc.status, res.Body.String())
		}
	}
	for _, tc := range []struct{ token, unit string }{{a, "달러"}, {b, "코인"}} {
		res := request(tc.token, http.MethodGet, "/api/me", "")
		if res.Code != 200 {
			t.Fatal(res.Body.String())
		}
		var me struct {
			RewardUnit string `json:"rewardUnit"`
			Configured bool   `json:"rewardUnitConfigured"`
		}
		if err := json.Unmarshal(res.Body.Bytes(), &me); err != nil || me.RewardUnit != tc.unit || !me.Configured {
			t.Fatalf("me: %+v %v", me, err)
		}
	}
	page, err := shop.Create(ctx, st.Pool, bidA, "달란트가 포함된 제목", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := shop.SetItems(ctx, st.Pool, bidA, page.ID, []shop.Item{{Name: "달란트 보상", Price: 20}}); err != nil {
		t.Fatal(err)
	}
	page, err = shop.OpenWeb(ctx, st.Pool, bidA, page.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/market/" + page.PublicToken + "/state", "/api/market/" + page.PublicToken} {
		res := request(viewer, http.MethodGet, path, "")
		if res.Code != 200 || !strings.Contains(res.Body.String(), `"rewardUnit":"달러"`) {
			t.Fatalf("market: %d %s", res.Code, res.Body.String())
		}
	}
	if _, err := scoring.Grant(ctx, st.Pool, bidA, viewerID, 100, "달란트라는 지급 사유"); err != nil {
		t.Fatal(err)
	}
	if _, err := shop.Checkout(ctx, st.Pool, page.PublicToken, viewerID, "달란트팬", "unit-test-order-a-1234", []shop.CheckoutLine{{ItemID: page.Items[0].ID, Quantity: 1}}); err != nil {
		t.Fatal(err)
	}
	other, err := shop.Create(ctx, st.Pool, bidB, "코인 시장", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := shop.SetItems(ctx, st.Pool, bidB, other.ID, []shop.Item{{Name: "달란트 보상", Price: 30}}); err != nil {
		t.Fatal(err)
	}
	other, err = shop.OpenWeb(ctx, st.Pool, bidB, other.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scoring.Grant(ctx, st.Pool, bidB, viewerID, 100, "fixture"); err != nil {
		t.Fatal(err)
	}
	if _, err := shop.Checkout(ctx, st.Pool, other.PublicToken, viewerID, "달란트팬", "unit-test-order-b-1234", []shop.CheckoutLine{{ItemID: other.Items[0].ID, Quantity: 1}}); err != nil {
		t.Fatal(err)
	}
	res := request(viewer, http.MethodGet, "/api/me/orders", "")
	var orders []shop.ViewerOrder
	if err := json.Unmarshal(res.Body.Bytes(), &orders); err != nil || len(orders) != 2 {
		t.Fatalf("orders: %s %v", res.Body.String(), err)
	}
	units := map[string]string{bidA: "달러", bidB: "코인"}
	for _, order := range orders {
		if order.RewardUnit != units[order.StreamerID] || order.Lines[0].ItemName != "달란트 보상" {
			t.Fatalf("wrong per-streamer unit/user content: %+v", order)
		}
	}
	res = request(a, http.MethodGet, "/api/points/log", "")
	if res.Code != 200 || !strings.Contains(res.Body.String(), "달러 /") || !strings.Contains(res.Body.String(), "달란트라는 지급 사유") {
		t.Fatalf("log unit/content: %s", res.Body.String())
	}
}
