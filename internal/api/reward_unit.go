package api

import (
	"errors"
	"net/http"

	"github.com/sooptalent/server/internal/store"
)

func (s *Server) handleSetRewardUnit(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		Unit string `json:"unit"`
	}
	if err := decode(w, r, &req); err != nil {
		return err
	}
	unit, err := s.Store.SetRewardUnit(r.Context(), bidOf(r), req.Unit)
	if errors.Is(err, store.ErrRewardUnitLocked) {
		return httpError{http.StatusConflict, err.Error()}
	}
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"rewardUnit": unit, "rewardUnitConfigured": true})
	return nil
}
