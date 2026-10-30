package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"
)

func TestValidateRewardUnit(t *testing.T) {
	for _, unit := range []string{"달란트", "달러", "코인", "USD", "$", "방송 코인", "T-코인"} {
		if got, err := ValidateRewardUnit(" " + unit + " "); err != nil || got != unit {
			t.Errorf("%q: %q %v", unit, got, err)
		}
	}
	for _, unit := range []string{"", " ", "<script>", "a\nb", "코인\u202e", "12345678901234567", "\xff"} {
		if _, err := ValidateRewardUnit(unit); err == nil {
			t.Errorf("invalid unit accepted: %q", unit)
		}
	}
}

func TestRewardUnitPersistenceAndIsolation(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL 미설정")
	}
	ctx := context.Background()
	st, err := Open(ctx, dsn, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// The test database is explicitly provided by TEST_DATABASE_URL, never .env.
	if err := st.Migrate(ctx, "../../migrations"); err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(ctx, "../../migrations"); err != nil {
		t.Fatalf("migration rerun: %v", err)
	}
	bid := fmt.Sprintf("unit_test_%d", time.Now().UnixNano())
	other := bid + "_other"
	defer st.Pool.Exec(ctx, `DELETE FROM streamer_reward_units WHERE bid=$1 OR bid=$2`, bid, other)
	if u, done, e := st.RewardUnit(ctx, bid); e != nil || u != DefaultRewardUnit || done {
		t.Fatalf("default: %q %v %v", u, done, e)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, unit := range []string{"달러", "코인"} {
		wg.Add(1)
		go func(u string) { defer wg.Done(); _, e := st.SetRewardUnit(ctx, bid, u); results <- e }(unit)
	}
	wg.Wait()
	close(results)
	success, conflict := 0, 0
	for e := range results {
		if e == nil {
			success++
		} else if errors.Is(e, ErrRewardUnitLocked) {
			conflict++
		} else {
			t.Fatal(e)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("simultaneous writes: %d success, %d conflict", success, conflict)
	}
	saved, done, err := st.RewardUnit(ctx, bid)
	if err != nil || !done {
		t.Fatalf("saved: %v %v", done, err)
	}
	if _, err := st.SetRewardUnit(ctx, bid, saved); err != nil {
		t.Fatalf("idempotent: %v", err)
	}
	if u, done, e := st.RewardUnit(ctx, other); e != nil || u != DefaultRewardUnit || done {
		t.Fatalf("tenant leak: %q %v %v", u, done, e)
	}
	if _, err := st.SetRewardUnit(ctx, other, DefaultRewardUnit); err != nil {
		t.Fatal(err)
	}
	if _, done, e := st.RewardUnit(ctx, other); e != nil || !done {
		t.Fatal("confirming the default must finish onboarding")
	}
}
