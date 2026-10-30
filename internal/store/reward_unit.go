package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/sooptalent/server/internal/apperr"
)

const DefaultRewardUnit = "달란트"

var ErrRewardUnitLocked = errors.New("보상 단위가 이미 설정되어 있습니다. 새로고침해 주세요")

func ValidateRewardUnit(raw string) (string, error) {
	unit := strings.TrimSpace(raw)
	if n := utf8.RuneCountInString(unit); n < 1 || n > 16 || !utf8.ValidString(unit) {
		return "", apperr.New("보상 단위는 1~16자로 입력해 주세요")
	}
	for _, r := range unit {
		if !unicode.IsLetter(r) && !unicode.IsNumber(r) && !unicode.Is(unicode.Sc, r) && r != ' ' && r != '-' && r != '_' {
			return "", apperr.New("보상 단위에는 한글·영문·숫자·통화 기호·공백·하이픈을 사용할 수 있습니다")
		}
	}
	return unit, nil
}

func (s *Store) RewardUnit(ctx context.Context, bid string) (string, bool, error) {
	if bid == "" {
		return DefaultRewardUnit, false, nil
	}
	var unit string
	err := s.Pool.QueryRow(ctx, `SELECT unit FROM streamer_reward_units WHERE bid=$1`, bid).Scan(&unit)
	if errors.Is(err, sql.ErrNoRows) {
		return DefaultRewardUnit, false, nil
	}
	if err != nil {
		return "", false, err
	}
	return unit, true, nil
}

// SetRewardUnit is first-write-wins, including simultaneous tabs. Repeating the
// same request is safe, but a second name never overwrites the original setting.
func (s *Store) SetRewardUnit(ctx context.Context, bid, raw string) (string, error) {
	unit, err := ValidateRewardUnit(raw)
	if err != nil {
		return "", err
	}
	if _, err := s.Pool.Exec(ctx, `INSERT INTO streamer_reward_units (bid,unit) VALUES ($1,$2)
 ON DUPLICATE KEY UPDATE bid=bid`, bid, unit); err != nil {
		return "", err
	}
	saved, _, err := s.RewardUnit(ctx, bid)
	if err != nil {
		return "", err
	}
	if saved != unit {
		return "", ErrRewardUnitLocked
	}
	return saved, nil
}
