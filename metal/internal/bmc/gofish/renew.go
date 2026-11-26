package gofish

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// A manager keeps its Redfish sessions in memory. A restart of the manager
// ends each one: after a firmware update, after a new HTTPS certificate,
// after a person restarts it. The idle timeout of the SessionService ends a
// session too. The client lives on, and gofish sends the ended token with
// each later request, so each one gets 401 until the client closes.
//
// renewal logs in again when a request that carried the session token gets
// 401, and sends that request again with the new token. gofish keeps its own
// token, so the guard writes the new one into each later request.
//
// Each refused login counts towards the lockout of the account. So a login
// that the service refuses ends the renewal for the life of the client. A
// client that changes the password of its own account, or turns it off,
// stops the renewal first: the old password can only be refused.
type renewal struct {
	send func(*http.Request) (*http.Response, error)

	mu       sync.Mutex
	sessions string // the Sessions collection, as a URL; "" when not armed
	path     string // its path
	username string
	password string
	token    string // the token in force; "" while the first one holds
	session  string // the URL of the session that token names
	off      bool
}

// arm turns the renewal on for a client that logged in with a session.
func (r *renewal) arm(sessions, username, password string) {
	u, err := url.Parse(sessions)
	if err != nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sessions, r.path, r.username, r.password = sessions, u.Path, username, password
}

// covers reports whether a request takes part in the renewal. The requests
// to the Sessions collection do not: a login carries no token, and the
// logout of an ended session needs no new one.
func (r *renewal) covers(req *http.Request) bool {
	if r == nil || req.Header.Get("X-Auth-Token") == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sessions != "" && !strings.HasPrefix(req.URL.Path, r.path)
}

// current is the token to send in place of sent.
func (r *renewal) current(sent string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.token != "" {
		return r.token
	}
	return sent
}

// renew logs in again after a request with used got 401, and returns the
// token to send that request again with. Requests that ran at the same time
// with used share the one login.
func (r *renewal) renew(ctx context.Context, used string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.off {
		return "", false
	}
	if r.token != "" && r.token != used {
		return r.token, true
	}
	token, session, status, err := r.login(ctx)
	if err != nil {
		// A refused login ends the renewal. Anything else (no answer, a
		// service that is not up yet) leaves it for the next 401.
		r.off = status == http.StatusUnauthorized || status == http.StatusForbidden
		return "", false
	}
	r.token, r.session = token, session
	return token, true
}

// login opens a session with the credentials of the client.
func (r *renewal) login(ctx context.Context) (token, session string, status int, err error) {
	body, err := json.Marshal(map[string]string{"UserName": r.username, "Password": r.password})
	if err != nil {
		return "", "", 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.sessions, bytes.NewReader(body))
	if err != nil {
		return "", "", 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := r.send(req)
	if err != nil {
		return "", "", 0, err
	}
	defer resp.Body.Close()
	answer, _ := io.ReadAll(resp.Body)
	token = resp.Header.Get("X-Auth-Token")
	if resp.StatusCode/100 != 2 || token == "" {
		return "", "", resp.StatusCode, fmt.Errorf("login: %s", resp.Status)
	}
	session = resp.Header.Get("Location")
	if session == "" {
		var doc struct {
			ODataID string `json:"@odata.id"`
		}
		_ = json.Unmarshal(answer, &doc)
		session = doc.ODataID
	}
	if base, err := url.Parse(r.sessions); err == nil && session != "" {
		if u, err := base.Parse(session); err == nil {
			session = u.String()
		}
	}
	return token, session, resp.StatusCode, nil
}

// changed stops the renewal when the client changes, or turns off, the
// account it logged in with.
func (r *renewal) changed(username string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if username == r.username {
		r.off = true
	}
}

// logout deletes the session that the renewal opened, and reports whether
// there was one. gofish's own logout would delete the first session, which
// has ended.
func (r *renewal) logout() bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.token == "" {
		return false
	}
	if r.session != "" {
		// The caller's context can have ended; the guard bounds the request.
		if req, err := http.NewRequestWithContext(context.Background(), http.MethodDelete, r.session, nil); err == nil {
			req.Header.Set("X-Auth-Token", r.token)
			if resp, err := r.send(req); err == nil {
				_ = resp.Body.Close()
			}
		}
	}
	r.token, r.session, r.off = "", "", true
	return true
}

// withToken is req with token in X-Auth-Token, and a new body when the
// request is sent again.
func withToken(req *http.Request, token string, body io.ReadCloser) *http.Request {
	if req.Header.Get("X-Auth-Token") == token && body == nil {
		return req
	}
	out := req.Clone(req.Context())
	out.Header.Set("X-Auth-Token", token)
	if body != nil {
		out.Body = body
	}
	return out
}

// replayBody is a new copy of the body of req, for a second send. A request
// without a body has nothing to copy; one whose body cannot be read again
// cannot be sent again.
func replayBody(req *http.Request) (io.ReadCloser, bool) {
	if req.Body == nil || req.Body == http.NoBody {
		return http.NoBody, true
	}
	if req.GetBody == nil {
		return nil, false
	}
	body, err := req.GetBody()
	return body, err == nil
}
