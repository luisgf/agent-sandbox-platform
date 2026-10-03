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

func TestExecPTYAndStdinProxiedToGuest(t *testing.T) {
	var gotPTY bool
	var gotStdin string
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	guest := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/exec":
			var body poddaemon.ExecRequest
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("exec body: %v", err)
			}
			gotPTY = body.PTY
			if r.URL.Query().Get("stream") != "1" {
				t.Errorf("stream=%s", r.URL.RawQuery)
			}
			w.Header().Set("Content-Type", "application/x-ndjson")
			_, _ = io.WriteString(w, "{\"type\":\"ready\",\"exec_id\":\"g1\"}\n{\"type\":\"exit\",\"exit_code\":0}\n")
		case "/v1/exec/stdin":
			raw, _ := io.ReadAll(r.Body)
			gotStdin = string(raw)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			http.NotFound(w, r)
		}
	})}
	go func() { _ = guest.Serve(ln) }()
	defer guest.Close()

	s := &Server{Pod: poddaemon.NewClientFromDialer(tcpDialer{addr: ln.Addr().String()})}
	proxy := httptest.NewServer(s.Handler())
	defer proxy.Close()

	resp, err := http.Post(proxy.URL+"/v1/internal/exec?stream=1", "application/json", strings.NewReader(`{"sandbox_id":"sb","cmd":["sh"],"pty":true,"rows":30,"cols":100}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !gotPTY {
		t.Fatal("pty was not forwarded")
	}
	if !strings.Contains(string(body), `"exec_id":"g1"`) {
		t.Fatalf("body=%s", body)
	}
	resp, err = http.Post(proxy.URL+"/v1/internal/exec/stdin", "application/json", strings.NewReader(`{"sandbox_id":"sb","exec_id":"g1","data":"ls\n","close":false}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("stdin status=%d %s", resp.StatusCode, b)
	}
	if !strings.Contains(gotStdin, `"exec_id":"g1"`) || !strings.Contains(gotStdin, `ls\n`) && !strings.Contains(gotStdin, "ls") {
		t.Fatalf("guest stdin=%s", gotStdin)
	}
}
