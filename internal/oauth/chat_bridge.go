package oauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

// ErrSOOPLiveRequired는 현재 SOOP Chat SDK가 본인 방송에만 접속할 수 있어
// 최초 방송국 ID 검증을 방송 중에만 할 수 있음을 나타낸다.
var ErrSOOPLiveRequired = errors.New("SOOP 방송을 켠 상태에서 다시 로그인해 주세요")

// resolveSOOPIdentityWithChatBridge는 브라우저 전용 Chat SDK의 getRoomInfo()
// 결과에서 토큰 소유 방송국의 bjId를 받는다. 승인된 API 두 개는 방송 상태와
// 방송국 프로필을 주지만 사용자 ID를 항상 주지는 않으므로 이 단계가 필요하다.
// 브리지는 같은 Pod의 127.0.0.1에만 열리고, 별도 공유 비밀로 인증된다.
func (r *Registry) resolveSOOPIdentityWithChatBridge(ctx context.Context, accessToken string) (string, error) {
	secret := strings.TrimSpace(os.Getenv("CHAT_BRIDGE_SECRET"))
	if secret == "" {
		return "", fmt.Errorf("내부 Chat SDK 브리지 인증키가 설정되지 않았습니다")
	}
	endpoint := strings.TrimRight(strings.TrimSpace(os.Getenv("CHAT_BRIDGE_URL")), "/")
	if endpoint == "" {
		endpoint = "http://127.0.0.1:8098"
	}
	payload, err := json.Marshal(struct {
		AccessToken string `json:"accessToken"`
	}{AccessToken: accessToken})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/identity", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("내부 Chat SDK 브리지에 연결하지 못했습니다: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return "", err
	}
	if resp.StatusCode == http.StatusConflict {
		return "", ErrSOOPLiveRequired
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("내부 Chat SDK 브리지 식별 실패 (HTTP %d)", resp.StatusCode)
	}
	var out struct {
		BJID string `json:"bjId"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("내부 Chat SDK 브리지 응답 파싱: %w", err)
	}
	id := strings.TrimSpace(out.BJID)
	if id == "" || strings.HasPrefix(id, "-") || strings.ContainsAny(id, "|/\\") {
		return "", fmt.Errorf("내부 Chat SDK 브리지가 유효한 방송국 ID를 돌려주지 않았습니다")
	}
	return id, nil
}
