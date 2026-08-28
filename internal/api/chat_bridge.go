package api

import (
	"crypto/subtle"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/sooptalent/server/internal/scoring"
	"github.com/sooptalent/server/internal/soop"
)

func (s *Server) chatBridgeRoutes(mux *http.ServeMux) {
	// Chat SDK는 about:blank가 아닌 앱의 정상적인 Origin에서 실행되어야 한다.
	// 이 문서는 브리지 프로세스만 열며 인증 정보·사용자 정보는 전혀 담지 않는다.
	mux.Handle("GET /internal/chat/sdk-host", s.h(s.handleChatSDKHost))
	mux.Handle("GET /internal/chat/targets", s.h(s.bridgeAuth(s.handleBridgeTargets)))
	mux.Handle("POST /internal/chat/event", s.h(s.bridgeAuth(s.handleBridgeEvent)))
	mux.Handle("POST /internal/chat/state", s.h(s.bridgeAuth(s.handleBridgeState)))
}

func (s *Server) handleChatSDKHost(w http.ResponseWriter, r *http.Request) error {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, err := w.Write([]byte("<!doctype html><html lang=\"ko\"><meta charset=\"utf-8\"><title>SoopTalent Chat SDK</title></html>"))
	return err
}

func (s *Server) bridgeAuth(next handlerFunc) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		expected := os.Getenv("CHAT_BRIDGE_SECRET")
		provided := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		if expected == "" || len(expected) != len(provided) ||
			subtle.ConstantTimeCompare([]byte(expected), []byte(provided)) != 1 {
			return httpError{http.StatusUnauthorized, "내부 채팅 브리지 인증에 실패했습니다."}
		}
		w.Header().Set("Cache-Control", "no-store")
		return next(w, r)
	}
}

func (s *Server) handleBridgeTargets(w http.ResponseWriter, r *http.Request) error {
	if s.OAuth == nil || !s.OAuth.SOOPTokenStorageReady() {
		return httpError{http.StatusServiceUnavailable, "SOOP OAuth 토큰 저장소가 준비되지 않았습니다."}
	}
	targets, err := s.OAuth.BridgeTargets(r.Context(), s.Store.Pool)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"targets": targets})
	return nil
}

var acceptedSDKActions = map[string]bool{
	// LOGIN은 공식 SDK가 채팅 서버 로그인 완료 직후 보내는 이벤트다.
	// 달란트 적립 대상은 아니지만, 브리지가 실제 연결 성공을 판별하는 데 쓴다.
	"LOGIN": true,
	"IN":    true, "JOIN": true, "OUT": true, "QUIT": true,
	"MESSAGE": true, "USERSTATUS_CHANGED": true,
	"MANAGER_MESSAGE": true,
	"BALLOON_GIFTED":  true, "ADBALLOON_GIFTED": true, "VIDEOBALLOON_GIFTED": true,
	"BATTLE_MISSION_GIFTED": true, "CHALLENGE_MISSION_GIFTED": true,
	"SUBSCRIBED": true, "SUBSCRIPTION_RENEWED": true, "SUBSCRIPTION_GIFTED": true,
}

func (s *Server) handleBridgeEvent(w http.ResponseWriter, r *http.Request) error {
	official, ok := s.Streamer.(OfficialChatHook)
	if !ok {
		return httpError{http.StatusServiceUnavailable, "수집기가 준비되지 않았습니다."}
	}
	var req struct {
		BID   string           `json:"bid"`
		Event scoring.SDKEvent `json:"event"`
	}
	if err := decodeLimit(w, r, &req, 1<<20); err != nil {
		return err
	}
	req.BID = soop.NormalizeUserID(req.BID)
	req.Event.Action = strings.ToUpper(strings.TrimSpace(req.Event.Action))
	if !soop.IsUserID(req.BID) || !acceptedSDKActions[req.Event.Action] ||
		req.Event.ID == "" || len(req.Event.ID) > 128 {
		return httpError{http.StatusBadRequest, "공식 채팅 이벤트 형식이 올바르지 않습니다."}
	}
	req.Event.Received = time.Now()
	if err := official.IngestSDK(r.Context(), req.BID, req.Event); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) handleBridgeState(w http.ResponseWriter, r *http.Request) error {
	official, ok := s.Streamer.(OfficialChatHook)
	if !ok {
		return httpError{http.StatusServiceUnavailable, "수집기가 준비되지 않았습니다."}
	}
	var req struct{ BID, Phase, BroadNo, Detail, Error string }
	if err := decode(w, r, &req); err != nil {
		return err
	}
	req.BID = soop.NormalizeUserID(req.BID)
	allowed := map[string]bool{soop.PhaseConnecting: true, soop.PhaseConnected: true,
		soop.PhaseOffline: true, soop.PhaseRetrying: true, soop.PhaseStopped: true}
	if !soop.IsUserID(req.BID) || !allowed[req.Phase] {
		return httpError{http.StatusBadRequest, "공식 채팅 연결 상태가 올바르지 않습니다."}
	}
	official.SetSDKState(req.BID, req.Phase, req.BroadNo,
		strings.TrimSpace(req.Detail), strings.TrimSpace(req.Error))
	w.WriteHeader(http.StatusNoContent)
	return nil
}
