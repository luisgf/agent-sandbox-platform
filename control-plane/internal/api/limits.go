package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	// maxBodyBytes bounds the JSON body of an ordinary request.
	maxBodyBytes = 1 << 20
	// maxSmallBodyBytes bounds the body of a call whose payload is a few
	// fields: enroll, register, claim, token mint, attestation.
	maxSmallBodyBytes = 64 << 10
	// maxHeartbeatBytes bounds the optional body of a node heartbeat.
	maxHeartbeatBytes = 4 << 10
)

// bodyReadTimeout is how long a client has to send a request body. A variable
// so tests can shorten it.
var bodyReadTimeout = 30 * time.Second

// readBody reads a request body of at most max bytes. A larger one is
// answered 413 and the connection is closed; one that cannot be read, 400.
// It reports false once it has answered.
//
// The read deadline covers the body only. The server has no ReadTimeout on
// purpose: it would also fire on the background read that watches a streamed
// exec response for a closed client, and cancel the request context of a
// command that is still running.
func readBody(w http.ResponseWriter, r *http.Request, max int64) ([]byte, bool) {
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(bodyReadTimeout))
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, max))
	if err != nil {
		// The deadline stays: after a timeout it has passed, which is what lets
		// the server give up draining the rest of the body before it answers,
		// and after a too-large body the server closes the connection anyway.
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("request body too large (limit %d bytes)", max))
			return nil, false
		}
		writeError(w, http.StatusBadRequest, "invalid body")
		return nil, false
	}
	_ = rc.SetReadDeadline(time.Time{})
	return body, true
}

// decodeJSON reads a JSON body of at most max bytes into v, answering 413 or
// 400 and reporting false when it cannot.
func decodeJSON(w http.ResponseWriter, r *http.Request, max int64, v any) bool {
	body, ok := readBody(w, r, max)
	if !ok {
		return false
	}
	if err := json.Unmarshal(body, v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}
	return true
}
