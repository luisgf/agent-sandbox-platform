package execproxy

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/poddaemon"
)

// as_root travels from the control plane to pod-daemon on every exec path, and
// is absent when it was not asked for: pod-daemon then runs the command as the
// workspace owner or its default user.
func TestAsRootReachesTheGuest(t *testing.T) {
	var seen []poddaemon.ExecRequest
	var raws []string
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	guest := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body poddaemon.ExecRequest
		_ = json.Unmarshal(raw, &body)
		seen = append(seen, body)
		raws = append(raws, string(raw))
		if r.URL.Query().Get("stream") == "1" {
			w.Header().Set("Content-Type", "application/x-ndjson")
			_, _ = io.WriteString(w, "{\"type\":\"exit\",\"exit_code\":0}\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"stdout":"","stderr":"","exit_code":0}`))
	})}
	go func() { _ = guest.Serve(ln) }()
	defer guest.Close()

	proxy := httptest.NewServer((&Server{Pod: poddaemon.NewClientFromDialer(tcpDialer{addr: ln.Addr().String()})}).Handler())
	defer proxy.Close()

	post := func(path, body string) {
		t.Helper()
		resp, err := http.Post(proxy.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status %d", path, resp.StatusCode)
		}
	}
	post("/v1/internal/exec", `{"sandbox_id":"sb","cmd":["id"],"as_root":true}`)
	post("/v1/internal/exec", `{"sandbox_id":"sb","cmd":["id"]}`)
	post("/v1/internal/exec?stream=1", `{"sandbox_id":"sb","cmd":["id"],"as_root":true,"pty":true}`)
	post("/v1/internal/exec?stream=1", `{"sandbox_id":"sb","cmd":["id"],"stdin_stream":true}`)

	if len(seen) != 4 {
		t.Fatalf("guest saw %d requests", len(seen))
	}
	for i, want := range []bool{true, false, true, false} {
		if seen[i].AsRoot != want {
			t.Errorf("request %d: as_root=%v, want %v (%s)", i, seen[i].AsRoot, want, raws[i])
		}
	}
	if strings.Contains(raws[1], "as_root") || strings.Contains(raws[3], "as_root") {
		t.Errorf("a command that did not ask for root sends the field anyway: %s %s", raws[1], raws[3])
	}
}

// timeout_seconds reaches pod-daemon as timeout_secs, so a buffered exec runs for
// as long as it was allowed to.
func TestTimeoutSecondsReachesTheGuest(t *testing.T) {
	var got []poddaemon.ExecRequest
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	guest := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body poddaemon.ExecRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		got = append(got, body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"stdout":"","stderr":"","exit_code":0}`))
	})}
	go func() { _ = guest.Serve(ln) }()
	defer guest.Close()
	proxy := httptest.NewServer((&Server{Pod: poddaemon.NewClientFromDialer(tcpDialer{addr: ln.Addr().String()})}).Handler())
	defer proxy.Close()
	for _, body := range []string{
		`{"sandbox_id":"sb","cmd":["make"],"timeout_seconds":1800}`,
		`{"sandbox_id":"sb","cmd":["make"]}`,
	} {
		resp, err := http.Post(proxy.URL+"/v1/internal/exec", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	if len(got) != 2 || got[0].TimeoutSecs != 1800 || got[1].TimeoutSecs != 0 {
		t.Fatalf("guest saw %+v", got)
	}
}
