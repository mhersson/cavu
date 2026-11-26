package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/store"
)

// instanceSecrets are the secrets the settings tell are set or not, never what they are.
var instanceSecrets = []string{engine.SecretEndpointCredential, engine.SecretForgeToken, engine.SecretForgeAppKey}

// credentialJSON is a named credential as the settings show it: never its secret.
type credentialJSON struct {
	Name  string `json:"name"`
	Kind  string `json:"kind"`
	AppID int64  `json:"app_id,omitempty"`
}

// credentialIn is a credential a request writes whole; its kind follows from its fields.
type credentialIn struct {
	Token  string `json:"token"`
	AppID  int64  `json:"app_id"`
	AppKey string `json:"app_key"`
}

// settings answers with every settable key whose value is not empty, a value
// that is a JSON object as one, whether each instance secret is set, every
// named credential, the VAPID public key, every key's default, every switch's
// default and the most an image of a task may hold.
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

	credentials, err := s.Store.Credentials(r.Context())
	if err != nil {
		return err
	}

	named := make([]credentialJSON, len(credentials))
	for i, c := range credentials {
		named[i] = credentialJSON{Name: c.Name, Kind: c.Kind, AppID: c.AppID}
	}

	secrets := map[string]bool{}
	for _, name := range instanceSecrets {
		secrets[name] = slices.Contains(names, name)
	}

	reply(w, http.StatusOK, map[string]any{
		"settings": settings, "secrets": secrets, "forge_credentials": named, "push_key": s.PushKey,
		"defaults": engine.SettingDefaults(), "switches": engine.ProjectSwitches(store.Project{}),
		"image_max_bytes": engine.ImageMaxBytes, "task_images_max_bytes": engine.TaskImagesMaxBytes,
	})

	return nil
}

// setSettings checks every value before it writes any. A null or empty value
// unsets its key, and an empty secret is removed. A credential is written
// whole, and null removes it.
func (s *server) setSettings(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Settings map[string]json.RawMessage `json:"settings"`
		Secrets  map[string]string          `json:"secrets"`
		Forge    map[string]*credentialIn   `json:"forge_credentials"`
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

	changes := make(map[string]*engine.NewCredential, len(in.Forge))

	for name, c := range in.Forge {
		if c != nil {
			changes[name] = &engine.NewCredential{Token: c.Token, AppID: c.AppID, AppKey: c.AppKey}
		} else {
			changes[name] = nil
		}
	}

	if err := engine.ChangeCredentials(r.Context(), s.Store, s.Vault, changes); err != nil {
		return err
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
