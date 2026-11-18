package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/mhersson/cavu/internal/engine"
)

// instanceSecrets are the secrets the settings tell are set or not, never what they are.
var instanceSecrets = []string{engine.SecretEndpointCredential, engine.SecretForgeToken, engine.SecretForgeAppKey}

// settings answers with every settable key whose value is not empty, a value
// that is a JSON object as one, and whether each instance secret is set.
func (s *server) settings(w http.ResponseWriter, r *http.Request) error {
	all, err := s.Store.Settings(r.Context())
	if err != nil {
		return err
	}

	names, err := s.Store.SecretNames(r.Context())
	if err != nil {
		return err
	}

	settings := map[string]any{}

	for _, k := range engine.SettableKeys() {
		switch v := all[k]; {
		case json.Valid([]byte(v)) && strings.HasPrefix(strings.TrimSpace(v), "{"):
			settings[k] = json.RawMessage(v)
		case v != "":
			settings[k] = v
		}
	}

	secrets := map[string]bool{}
	for _, name := range instanceSecrets {
		secrets[name] = slices.Contains(names, name)
	}

	reply(w, http.StatusOK, map[string]any{"settings": settings, "secrets": secrets})

	return nil
}

// setSettings checks every value before it writes any. A null or empty value
// unsets its key, and an empty secret is removed.
func (s *server) setSettings(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Settings map[string]json.RawMessage `json:"settings"`
		Secrets  map[string]string          `json:"secrets"`
	}

	if err := decode(w, r, &in); err != nil {
		return err
	}

	values := make(map[string]string, len(in.Settings))

	for k, raw := range in.Settings {
		v, ok := settingText(raw)
		if !ok {
			return &apiError{http.StatusBadRequest, "settings " + strconv.Quote(k) + ": want a string, null or a JSON object"}
		}

		if err := engine.CheckSetting(k, v); err != nil {
			return &apiError{http.StatusBadRequest, err.Error()}
		}

		values[k] = v
	}

	for name := range in.Secrets {
		if !slices.Contains(instanceSecrets, name) {
			return &apiError{http.StatusBadRequest, "secrets " + strconv.Quote(name) + ": want " + strings.Join(instanceSecrets, " or ")}
		}
	}

	for k, v := range values {
		if err := s.Store.SetSetting(r.Context(), k, v); err != nil {
			return err
		}
	}

	if err := s.seal(r.Context(), in.Secrets, func(name string) string { return name }); err != nil {
		return err
	}

	return s.settings(w, r)
}

// settingText is the text a value is stored as: a string's own, an object's
// JSON, or "" for null. ok is false for any other JSON value.
func settingText(raw json.RawMessage) (text string, ok bool) {
	var v any

	_ = json.Unmarshal(raw, &v) // decode has read it as JSON

	switch v := v.(type) {
	case nil:
		return "", true
	case string:
		return v, true
	case map[string]any:
		var b bytes.Buffer

		_ = json.Compact(&b, raw)

		return b.String(), true
	}

	return "", false
}
