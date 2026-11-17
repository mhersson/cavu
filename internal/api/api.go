// Package api serves CAVU's REST API over HTTP under /api/, and checks the
// authority of every request in one place.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mhersson/cavu/internal/auth"
	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/store"
)

const (
	cookieName = "cavu_session"
	csrfHeader = "X-CAVU-CSRF"
	maxBody    = 1 << 20
)

// Deps is what the handler works on.
type Deps struct {
	Store   *store.Store
	Vault   *auth.Vault
	Auth    *auth.Service
	Loop    *engine.Loop              // records a person's actions; it needs no runner
	Nudge   func()                    // the scheduler's
	Waiting func(taskID int64) string // the scheduler's
	Version string
}

// route is one REST route. Only an open route is served without authority.
type route struct {
	pattern string
	open    bool
	serve   func(*server, http.ResponseWriter, *http.Request) error
}

var routes = []route{
	{"GET /api/health", true, (*server).health},
	{"POST /api/auth/login", true, (*server).login},
	{"POST /api/auth/logout", false, (*server).logout},
	{"GET /api/projects", false, (*server).projects},
	{"POST /api/projects", false, (*server).createProject},
	{"GET /api/projects/{id}", false, (*server).project},
	{"PATCH /api/projects/{id}", false, (*server).editProject},
	{"POST /api/projects/{id}/check", false, (*server).requestCheck},
	{"GET /api/settings", false, (*server).settings},
	{"PATCH /api/settings", false, (*server).setSettings},
}

// Routes lists every REST route as "METHOD /path". The handler is built from it.
func Routes() []string {
	out := make([]string, len(routes))
	for i, rt := range routes {
		out[i] = rt.pattern
	}

	return out
}

type server struct{ Deps }

// New returns the handler of every route.
func New(d Deps) http.Handler {
	s := &server{d}
	mux := http.NewServeMux()

	for _, rt := range routes {
		mux.HandleFunc(rt.pattern, func(w http.ResponseWriter, r *http.Request) {
			var err error
			if !rt.open {
				err = s.authorize(w, r)
			}

			if err == nil {
				err = rt.serve(s, w, r)
			}

			if err != nil {
				fail(w, r, err)
			}
		})
	}

	// Else the mux answers a request that matches no route in plain text.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fail(w, r, &apiError{http.StatusNotFound, "no route matches the method and the path"})
	})

	return mux
}

// apiError is a refusal that is the caller's: a status and one line saying why.
type apiError struct {
	status int
	text   string
}

func (e *apiError) Error() string { return e.text }

var errNoAuthority = &apiError{http.StatusUnauthorized, "log in, or send an API token as Authorization: Bearer TOKEN"}

// fail answers err as JSON: content the engine refuses is a 400, a write that
// conflicts with what is stored a 409, and a row that does not exist a 404.
// An error that is not the caller's is logged and answered without detail.
func fail(w http.ResponseWriter, r *http.Request, err error) {
	status, text := http.StatusInternalServerError, "internal error"

	switch e, ok := errors.AsType[*apiError](err); {
	case ok:
		status, text = e.status, e.text
	case errors.Is(err, engine.ErrRefused):
		status, text = http.StatusBadRequest, err.Error()
	case errors.Is(err, store.ErrConflict):
		status, text = http.StatusConflict, err.Error()
	case errors.Is(err, store.ErrNotFound):
		status, text = http.StatusNotFound, err.Error()
	default:
		slog.ErrorContext(r.Context(), "request failed", "route", r.Pattern, "err", err)
	}

	reply(w, status, map[string]string{"error": text})
}

func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// decode reads the body, at most maxBody of it, into v, refusing a field v does not have.
func decode(w http.ResponseWriter, r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()

	err := dec.Decode(v)
	if err == nil {
		// Decode stops at the end of the first value, so what follows it is read here.
		if err = dec.Decode(new(json.RawMessage)); errors.Is(err, io.EOF) {
			return nil
		}

		if err == nil {
			err = errors.New("more than one value")
		}
	}

	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		return &apiError{http.StatusRequestEntityTooLarge, "the body is larger than 1 MiB"}
	}

	return &apiError{http.StatusBadRequest, "the body is not the JSON this route takes: " + err.Error()}
}

// pathID reads the id in the path; one that is not a number names no row.
func pathID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return 0, &apiError{http.StatusNotFound, "the id in the path is not a number"}
	}

	return id, nil
}

// seal stores each secret under prefix and its name; an empty value removes the secret.
func (s *server) seal(ctx context.Context, prefix string, secrets map[string]string) error {
	for name, value := range secrets {
		var err error
		if value == "" {
			err = s.Vault.Delete(ctx, prefix+name)
		} else {
			err = s.Vault.Set(ctx, prefix+name, value)
		}

		if err != nil {
			return err
		}
	}

	return nil
}

// authorize is the one check of authority: a bearer token the login service
// accepts, else the session cookie.
func (s *server) authorize(w http.ResponseWriter, r *http.Request) error {
	if token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		if valid, err := s.Auth.Bearer(r.Context(), token); err != nil || valid {
			return err
		}
	}

	c, err := r.Cookie(cookieName)
	if err != nil {
		return errNoAuthority
	}

	// SameSite does not tell ports apart, so a page on another port of this host
	// sends the cookie too; it cannot add a header without CORS, which is never granted.
	if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get(csrfHeader) == "" {
		return &apiError{http.StatusForbidden, "a request with the session cookie needs the " + csrfHeader + " header"}
	}

	ok, renewed, err := s.Auth.Session(r.Context(), c.Value)
	if err != nil {
		return err
	}

	if !ok {
		return errNoAuthority
	}

	if !renewed.IsZero() {
		setCookie(w, r, c.Value, renewed)
	}

	return nil
}

// setCookie sends the session cookie, Secure when the request came over TLS,
// directly or through a proxy that says so.
func setCookie(w http.ResponseWriter, r *http.Request, value string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure follows the request, so a login over plain HTTP on a LAN works
		Name: cookieName, Value: value, Path: "/", Expires: expires, HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
	})
}

func (s *server) health(w http.ResponseWriter, _ *http.Request) error {
	reply(w, http.StatusOK, map[string]string{"status": "ok", "version": s.Version})

	return nil
}

func (s *server) login(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Name     string `json:"name"`
		Password string `json:"password"`
	}

	if err := decode(w, r, &in); err != nil {
		return err
	}

	// The limiter counts by the host alone: the port is new on every
	// connection, and a forwarded header is whatever the client wrote.
	host, _, _ := net.SplitHostPort(r.RemoteAddr)

	cookie, expires, err := s.Auth.Login(r.Context(), in.Name, in.Password, host)
	if limited, ok := errors.AsType[*auth.LimitedError](err); ok {
		w.Header().Set("Retry-After", strconv.FormatInt(int64((limited.RetryAfter+time.Second-1)/time.Second), 10))

		return &apiError{http.StatusTooManyRequests, "too many failed logins; try again after Retry-After seconds"}
	}

	if errors.Is(err, auth.ErrBadLogin) {
		return &apiError{http.StatusUnauthorized, "wrong name or password"}
	}

	if err != nil {
		return err
	}

	setCookie(w, r, cookie, expires)
	w.WriteHeader(http.StatusNoContent)

	return nil
}

// logout ends the session of the cookie the request carries and clears the cookie.
func (s *server) logout(w http.ResponseWriter, r *http.Request) error {
	if c, err := r.Cookie(cookieName); err == nil {
		if err := s.Auth.Logout(r.Context(), c.Value); err != nil {
			return err
		}

		setCookie(w, r, "", time.Unix(0, 0))
	}

	w.WriteHeader(http.StatusNoContent)

	return nil
}
