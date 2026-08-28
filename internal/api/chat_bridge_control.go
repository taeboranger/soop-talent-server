package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"time"
)

// requestBridgeReconcile는 같은 Pod의 Chat SDK 브리지에 설정 변경을 즉시
// 알린다. 브리지가 잠시 내려가 있거나 구 버전이어도 채팅 스위치 저장 자체는
// 성공해야 하므로, 실패는 로그에만 남기고 다음 30초 주기 동기화에 맡긴다.
func (s *Server) requestBridgeReconcile(ctx context.Context, bid string) {
	secret := strings.TrimSpace(os.Getenv("CHAT_BRIDGE_SECRET"))
	endpoint := strings.TrimRight(strings.TrimSpace(os.Getenv("CHAT_BRIDGE_URL")), "/")
	if secret == "" || endpoint == "" {
		return
	}
	body, err := json.Marshal(map[string]string{"bid": bid})
	if err != nil {
		return
	}
	requestCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, endpoint+"/reconcile", bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.Log.Warn("Chat SDK 브리지 즉시 동기화 요청 실패", "bid", bid, "err", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		s.Log.Warn("Chat SDK 브리지 즉시 동기화 거부", "bid", bid, "status", resp.StatusCode)
	}
}
