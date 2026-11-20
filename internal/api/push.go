package api

import (
	"fmt"
	"net/http"

	"github.com/mhersson/cavu/internal/push"
	"github.com/mhersson/cavu/internal/store"
)

// putPush registers a browser's push subscription, or updates the one with its
// endpoint, and records whether the browser's page has focus.
func (s *server) putPush(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Endpoint string `json:"endpoint"`
		Keys     struct {
			P256DH string `json:"p256dh"`
			Auth   string `json:"auth"`
		} `json:"keys"`
		Origin  string `json:"origin"`
		Focused bool   `json:"focused"`
	}

	if err := decode(w, r, &in); err != nil {
		return err
	}

	sub := store.PushSubscription{Endpoint: in.Endpoint, P256DH: in.Keys.P256DH, Auth: in.Keys.Auth, Origin: in.Origin, CreatedAt: s.Loop.Now()}
	if err := push.Check(sub); err != nil {
		return &apiError{http.StatusBadRequest, err.Error()}
	}

	id, err := s.Store.PutPushSubscription(r.Context(), sub)
	if err != nil {
		return err
	}

	s.Focus.Set(id, in.Focused)
	reply(w, http.StatusOK, map[string]int64{"id": id})

	return nil
}

func (s *server) removePush(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}

	if err := s.Store.RemovePushSubscription(r.Context(), id); err != nil {
		return fmt.Errorf("push subscription %d: %w", id, err)
	}

	w.WriteHeader(http.StatusNoContent)

	return nil
}
