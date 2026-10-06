package api

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Agent limits short enough to test. The fake agents below go on for longer than
// either, so a call that works shows the limit did not apply to it.
const (
	testResponseHeaderTimeout = 200 * time.Millisecond
	testBufferedExecTimeout   = 200 * time.Millisecond
)

// agentTransports are the two clients the control plane reaches an agent with.
var agentTransports = []struct {
	name string
	mtls bool
}{
	{name: "same-host plain HTTP", mtls: false},
	{name: "mutual TLS", mtls: true},
}

// newExecTimeoutFixture puts a control plane with the short limits above in front
// of a fake agent serving h. It returns the control plane's URL and the id of a
// sandbox on that agent. With mtls the agent is reached the way --agent-tls-listen
// serves it (mutual TLS, HTTP/2), otherwise as a same-host agent.
func newExecTimeoutFixture(t *testing.T, mtls bool, h http.Handler) (cpURL, sandboxID string) {
	t.Helper()
	f := newMTLSExecFixture(t)
	timeouts := defaultAgentTimeouts()
	timeouts.ResponseHeader = testResponseHeaderTimeout
	f.srv.Client = newAgentHTTPClient(timeouts)
	f.srv.Agents.Timeouts = timeouts
	f.srv.BufferedExecTimeout = testBufferedExecTimeout
	var agent *httptest.Server
	if mtls {
		issued, err := f.ca.IssueNodeCert("n1", []string{"127.0.0.1"}, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		agent = startMTLSAgent(t, f.ca, issued.CertPEM, issued.KeyPEM, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.ProtoMajor != 2 {
				t.Errorf("control plane reached the agent over %s, want HTTP/2 as with --agent-tls-listen", r.Proto)
			}
			h.ServeHTTP(w, r)
		}))
	} else {
		agent = httptest.NewServer(h)
		t.Cleanup(agent.Close)
	}
	sandboxID = f.sandboxOn("n1", agent.URL)
	cp := httptest.NewServer(f.mux)
	t.Cleanup(cp.Close)
	return cp.URL, sandboxID
}

// postExec calls exec on the control plane, streamed or buffered. hangUp drops the
// call as a caller that goes away does. The call also gives up after 10s, so a
// regression fails instead of hanging.
func postExec(t *testing.T, cpURL, sandboxID string, stream bool) (resp *http.Response, hangUp context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	url := cpURL + "/v1/sandboxes/" + sandboxID + "/exec"
	if stream {
		url += "?stream=1"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(`{"cmd":["sh"]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp, cancel
}

// A stream that goes on well past every limit, with silences longer than them,
// reaches the caller whole: the response-header timeout stops counting once the
// agent answers, and the buffered limit does not apply. Before, the client's
// overall timeout cut the stream while output was still flowing.
func TestExecStreamOutlastsTheAgentTimeouts(t *testing.T) {
	const gap = 250 * time.Millisecond
	lines := []string{
		`{"type":"stdout","data":"0\n"}` + "\n",
		`{"type":"stdout","data":"1\n"}` + "\n",
		`{"type":"stdout","data":"2\n"}` + "\n",
		`{"type":"stdout","data":"3\n"}` + "\n",
		`{"type":"exit","exit_code":0}` + "\n",
	}
	want := strings.Join(lines, "") // 1s of stream in all
	agent := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		for i, line := range lines {
			if i > 0 {
				select {
				case <-time.After(gap):
				case <-r.Context().Done():
					return
				}
			}
			_, _ = io.WriteString(w, line)
			w.(http.Flusher).Flush()
		}
	})
	for _, tc := range agentTransports {
		t.Run(tc.name, func(t *testing.T) {
			cpURL, id := newExecTimeoutFixture(t, tc.mtls, agent)
			start := time.Now()
			resp, _ := postExec(t, cpURL, id, true)
			got, err := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusOK || err != nil || string(got) != want {
				t.Fatalf("after %s: exec=%d err=%v\n got %q\nwant %q", time.Since(start), resp.StatusCode, err, got, want)
			}
		})
	}
}

// An agent that takes the call but never starts answering costs the caller a 502
// once the response-header timeout passes. It is the only limit on a stream
// before the agent answers.
func TestExecStreamFailsWhenTheAgentNeverAnswers(t *testing.T) {
	for _, tc := range agentTransports {
		t.Run(tc.name, func(t *testing.T) {
			release := make(chan struct{})
			cpURL, id := newExecTimeoutFixture(t, tc.mtls, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				select {
				case <-r.Context().Done():
				case <-release:
				}
			}))
			t.Cleanup(func() { close(release) })
			start := time.Now()
			resp, _ := postExec(t, cpURL, id, true)
			body, _ := io.ReadAll(resp.Body)
			elapsed := time.Since(start)
			if resp.StatusCode != http.StatusBadGateway || !strings.Contains(string(body), "timeout awaiting response headers") {
				t.Fatalf("exec=%d %s, want 502 from the response-header timeout", resp.StatusCode, body)
			}
			if elapsed < testResponseHeaderTimeout {
				t.Fatalf("502 after %s, before the %s response-header timeout", elapsed, testResponseHeaderTimeout)
			}
		})
	}
}

// With no overall timeout, a caller that hangs up is what ends a stream early: the
// call to the agent carries the caller's request context.
func TestExecStreamCallerHangingUpCancelsTheAgentCall(t *testing.T) {
	for _, tc := range agentTransports {
		t.Run(tc.name, func(t *testing.T) {
			release := make(chan struct{})
			canceled := make(chan struct{})
			cpURL, id := newExecTimeoutFixture(t, tc.mtls, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// An HTTP/1 server notices a hang-up only once the request body is read.
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "application/x-ndjson")
				_, _ = io.WriteString(w, `{"type":"ready","exec_id":"e1"}`+"\n")
				w.(http.Flusher).Flush()
				select {
				case <-r.Context().Done():
					close(canceled)
				case <-release:
				}
			}))
			t.Cleanup(func() { close(release) })
			resp, hangUp := postExec(t, cpURL, id, true)
			if line, err := bufio.NewReader(resp.Body).ReadString('\n'); err != nil || !strings.Contains(line, "ready") {
				t.Fatalf("first line %q, err=%v", line, err)
			}
			hangUp()
			select {
			case <-canceled:
			case <-time.After(5 * time.Second):
				t.Fatal("the agent call was still open 5s after the caller hung up")
			}
		})
	}
}

// A buffered exec keeps an overall limit: an agent that starts answering and then
// stalls cannot hold the call open past BufferedExecTimeout.
func TestBufferedExecKeepsAnOverallTimeout(t *testing.T) {
	for _, tc := range agentTransports {
		t.Run(tc.name, func(t *testing.T) {
			release := make(chan struct{})
			cpURL, id := newExecTimeoutFixture(t, tc.mtls, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"stdout":"partial`)
				w.(http.Flusher).Flush()
				select {
				case <-r.Context().Done():
				case <-release:
				}
			}))
			t.Cleanup(func() { close(release) })
			start := time.Now()
			resp, _ := postExec(t, cpURL, id, false)
			body, _ := io.ReadAll(resp.Body)
			elapsed := time.Since(start)
			if resp.StatusCode != http.StatusBadGateway || !strings.Contains(string(body), "deadline exceeded") {
				t.Fatalf("exec=%d %s, want 502 from the buffered exec timeout", resp.StatusCode, body)
			}
			if elapsed < testBufferedExecTimeout {
				t.Fatalf("502 after %s, before the %s buffered exec timeout", elapsed, testBufferedExecTimeout)
			}
		})
	}
}
