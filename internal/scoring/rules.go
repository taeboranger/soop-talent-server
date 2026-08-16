package scoring

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/sooptalent/server/internal/store"
)

// Rules는 달란트 기준 전체다.
// 스트리머가 아무것도 건드리지 않아도 동작하도록 모든 항목에 기본값이 있다.
type Rules struct {
	Stay      StayRules      `json:"stay"`
	Chat      ChatRules      `json:"chat"`
	Rank      RankRules      `json:"rank"`
	Donation  DonationRules  `json:"donation"`
	Subscribe SubscribeRules `json:"subscribe"`
	Fanclub   FanclubRules   `json:"fanclub"`
	Manual    ManualRules    `json:"manual"`
}

type StayRules struct {
	IntervalMinutes int `json:"intervalMinutes"` // 몇 분마다 지급할지
	Points          int `json:"points"`          // 회당 달란트
	GraceSeconds    int `json:"graceSeconds"`    // 이 시간 안에 다시 보이면 같은 세션
}

type ChatRules struct {
	Points       int `json:"points"`
	MinLength    int `json:"minLength"`    // 이보다 짧으면 달란트 없음
	PerMinuteCap int `json:"perMinuteCap"` // 분당 인정 건수 (도배 방지)
	DailyCap     int `json:"dailyCap"`     // 채팅으로 하루에 얻을 수 있는 최대 달란트
}

type RankRules struct {
	TopPoints   int  `json:"topPoints"`   // 1위 달란트. 100위까지 선형 감소
	EnableWatch bool `json:"enableWatch"` // 참여시간 랭킹 반영
	EnableChat  bool `json:"enableChat"`  // 채팅입력 랭킹 반영
}

type DonationRules struct {
	StarBalloonRate int `json:"starBalloonRate"` // 별풍선 1개당 달란트
	AdBalloonRate   int `json:"adBalloonRate"`   // 애드벌룬 1개당 달란트
}

type SubscribeRules struct {
	Base      int `json:"base"`
	PerMonth  int `json:"perMonth"`
	MaxMonths int `json:"maxMonths"`
}

type FanclubRules struct {
	JoinPoints int `json:"joinPoints"` // 팬클럽 가입 시 1회
}

type ManualRules struct {
	PostPoints    int `json:"postPoints"`
	CommentPoints int `json:"commentPoints"`
}

// DefaultRules는 DESIGN.md 부록의 기본 달란트표와 일치한다.
func DefaultRules() Rules {
	return Rules{
		Stay:      StayRules{IntervalMinutes: 10, Points: 5, GraceSeconds: 120},
		Chat:      ChatRules{Points: 2, MinLength: 2, PerMinuteCap: 3, DailyCap: 200},
		Rank:      RankRules{TopPoints: 100, EnableWatch: true, EnableChat: true},
		Donation:  DonationRules{StarBalloonRate: 10, AdBalloonRate: 10},
		Subscribe: SubscribeRules{Base: 300, PerMonth: 50, MaxMonths: 24},
		Fanclub:   FanclubRules{JoinPoints: 500},
		Manual:    ManualRules{PostPoints: 50, CommentPoints: 10},
	}
}

// RankPoints는 순위에 따른 가산점이다. 1위가 TopPoints, 100위가 1점이 되도록 선형 감소한다.
func (r RankRules) RankPoints(rank int) int {
	const maxRank = 100 // SOOP이 돌려주는 랭킹 상한
	if rank < 1 || rank > maxRank || r.TopPoints <= 0 {
		return 0
	}
	// ceil(TopPoints * (1 - (rank-1)/maxRank))
	num := r.TopPoints * (maxRank - rank + 1)
	pts := (num + maxRank - 1) / maxRank
	if pts < 1 {
		return 1
	}
	return pts
}

const rulesKey = "rules"

// Load는 저장된 규칙을 읽는다. 없거나 일부 항목이 비어 있으면 기본값으로 채운다.
// 새 항목을 추가해도 기존 설정이 깨지지 않는다.
func Load(ctx context.Context, pool *store.DB, bid string) (Rules, error) {
	out := DefaultRules()

	var raw []byte
	err := pool.QueryRow(ctx,
		`SELECT value FROM scoring_rules WHERE bid = $1 AND key = $2`, bid, rulesKey).Scan(&raw)
	if err != nil {
		// 행이 없으면 기본값을 쓴다.
		return out, nil
	}
	if len(raw) == 0 {
		return out, nil
	}
	// 기본값 위에 덮어쓰므로 저장되지 않은 필드는 기본값이 유지된다.
	if err := json.Unmarshal(raw, &out); err != nil {
		return DefaultRules(), fmt.Errorf("규칙 파싱: %w", err)
	}
	return out, nil
}

func Save(ctx context.Context, pool *store.DB, bid string, r Rules) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = pool.Exec(ctx, `
		INSERT INTO scoring_rules (bid, key, value) VALUES ($1, $2, $3)
		ON DUPLICATE KEY UPDATE value = VALUES(value), updated_at = NOW(6)`,
		bid, rulesKey, b)
	return err
}

func Reset(ctx context.Context, pool *store.DB, bid string) (Rules, error) {
	def := DefaultRules()
	return def, Save(ctx, pool, bid, def)
}
