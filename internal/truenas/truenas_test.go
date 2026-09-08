package truenas

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fakeMiddleware is an in-process TrueNAS middleware: just enough JSON-RPC
// for the client tests, with switches for the failure modes the live
// recon found.
type fakeMiddleware struct {
	srv *httptest.Server
	url string // what a client dials

	mu              sync.Mutex
	accepted        int              // websocket connections taken
	versionReplies  int              // system.version answers so far
	logins          []map[string]any // captured login params
	paramViolations []string         // requests whose params was not an array

	snapshots map[string][]Snapshot // dataset -> canned list for pool.snapshot.query
	created   map[string]string     // ids this fake accepted via pool.snapshot.create

	conns []*websocket.Conn // every accepted connection, for test-side kills

	failLogin      bool
	notifications  int           // unsolicited messages sent before the version reply
	closeAfterPing bool          // drop the connection after the first keepalive
	delay          time.Duration // post-login calls stall this long before replying
}

func newFake(t *testing.T, tls bool) *fakeMiddleware {
	t.Helper()
	f := &fakeMiddleware{}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		ctx := r.Context()

		f.mu.Lock()
		f.accepted++
		f.conns = append(f.conns, conn)
		f.mu.Unlock()

		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var req struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
			}
			if err := json.Unmarshal(data, &req); err != nil {
				return
			}

			// The contract under test: params is always a JSON array —
			// and for known zero-arg methods, exactly an empty one. The
			// live server caught a params-in-a-params bug that a simple
			// "starts with [" check would have let through.
			p := strings.TrimSpace(string(req.Params))
			f.mu.Lock()
			if p == "" || p[0] != '[' {
				f.paramViolations = append(f.paramViolations, req.Method+": not an array: "+p)
			}
			if req.Method == "system.version" || req.Method == "core.ping" {
				if p != "[]" {
					f.paramViolations = append(f.paramViolations, req.Method+": zero-arg method got "+p)
				}
			}
			f.mu.Unlock()

			var result any
			switch req.Method {
			case "auth.login_ex":
				// params is [{mechanism, username, api_key}] — an array
				// wrapping the login object, per the login_ex schema.
				var logins []map[string]any
				_ = json.Unmarshal(req.Params, &logins)
				if len(logins) > 0 {
					f.mu.Lock()
					f.logins = append(f.logins, logins[0])
					f.mu.Unlock()
				}
				if f.failLogin {
					result = map[string]any{"response_type": "AUTH_ERR"}
				} else {
					result = map[string]any{"response_type": "SUCCESS"}
				}
			case "system.version":
				// Post-login calls can be made to stall, for the timeout
				// test — but never the handshake's own version call, or
				// the client could never reach Connected in the first place.
				f.mu.Lock()
				f.versionReplies++
				d := f.delay
				if f.versionReplies <= 1 {
					d = 0
				}
				f.mu.Unlock()
				if d > 0 {
					time.Sleep(d)
				}
				// Unsolicited traffic must never break the pending call.
				for i := 0; i < f.notifications; i++ {
					note, _ := json.Marshal(map[string]any{
						"jsonrpc": "2.0", "method": "job.progress", "params": map[string]any{"percent": i},
					})
					_ = conn.Write(ctx, websocket.MessageText, note)
				}
				result = "TrueNAS-TEST-25.10.5"
			case "core.ping":
				result = nil
				f.reply(conn, req.ID, result)
				f.mu.Lock()
				drop := f.closeAfterPing
				f.mu.Unlock()
				if drop {
					conn.CloseNow()
					return
				}
				continue
			case "pool.snapshot.query":
				// params: [[[dataset, =, name]], {options}] — the query
				// shape the middleware documents and Pademelon sends.
				var wrapper []json.RawMessage
				_ = json.Unmarshal(req.Params, &wrapper)
				var filters [][]any
				_ = json.Unmarshal(wrapper[0], &filters)
				ds, _ := filters[0][2].(string)
				f.mu.Lock()
				canned := f.snapshots[ds]
				f.mu.Unlock()
				items := []map[string]any{}
				for _, s := range canned {
					items = append(items, map[string]any{
						"id": s.ID, "dataset": s.Dataset, "snapshot_name": s.Name, "pool": "nvme",
						"properties": map[string]any{
							"creation":   map[string]any{"rawvalue": strconv.FormatInt(s.Created, 10)},
							"used":       map[string]any{"rawvalue": strconv.FormatUint(s.Used, 10)},
							"referenced": map[string]any{"rawvalue": strconv.FormatUint(s.Referenced, 10)},
						},
					})
				}
				result = items
			case "pool.snapshot.create":
				// params: [{dataset, name}] — the shape live recon proved.
				var creates []struct {
					Dataset string `json:"dataset"`
					Name    string `json:"name"`
				}
				_ = json.Unmarshal(req.Params, &creates)
				if len(creates) == 0 {
					f.replyError(conn, req.ID)
					continue
				}
				id := creates[0].Dataset + "@" + creates[0].Name
				f.mu.Lock()
				if f.created == nil {
					f.created = map[string]string{}
				}
				f.created[id] = id
				f.mu.Unlock()
				result = map[string]any{"id": id, "dataset": creates[0].Dataset, "snapshot_name": creates[0].Name}
			default:
				f.replyError(conn, req.ID)
				continue
			}
			f.reply(conn, req.ID, result)
		}
	})

	if tls {
		f.srv = httptest.NewTLSServer(handler)
		f.url = "wss://" + strings.TrimPrefix(f.srv.URL, "https://") + apiPath
	} else {
		f.srv = httptest.NewServer(handler)
		f.url = "ws://" + strings.TrimPrefix(f.srv.URL, "http://") + apiPath
	}
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeMiddleware) reply(conn *websocket.Conn, id json.RawMessage, result any) {
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": id, "result": result,
	})
	_ = conn.Write(context.Background(), websocket.MessageText, body)
}

func (f *fakeMiddleware) replyError(conn *websocket.Conn, id json.RawMessage) {
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": id,
		"error": map[string]any{"code": -32001, "message": "Method call error", "data": map[string]any{"errname": "EACCES"}},
	})
	_ = conn.Write(context.Background(), websocket.MessageText, body)
}

// dropConnections closes every live fake connection directly —
// httptest's CloseClientConnections does not reach hijacked websocket
// sockets, so tests that simulate a NAS restart call this.
func (f *fakeMiddleware) dropConnections() {
	f.mu.Lock()
	conns := f.conns
	f.conns = nil
	f.mu.Unlock()
	for _, conn := range conns {
		conn.CloseNow()
	}
}

func (f *fakeMiddleware) count() (conns int, violations []string, logins []map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.accepted, f.paramViolations, f.logins
}

// testConfig is the fast-forward config every client test uses: tiny
// timers so the reconnect, keepalive and retry-floor paths run in
// milliseconds.
func testConfig(f *fakeMiddleware) Config {
	return Config{
		Host:       f.url,
		Username:   "pademelon",
		APIKey:     "test-key",
		Timeout:    500 * time.Millisecond,
		Keepalive:  50 * time.Millisecond,
		Backoff:    10 * time.Millisecond,
		RetryFloor: 10 * time.Millisecond,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// waitFor polls the status until cond holds, failing with the last state
// if the deadline passes.
func waitFor(t *testing.T, c *Client, cond func(Status) bool) Status {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if st := c.Status(); cond(st) {
			return st
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("status condition not met in time; last: %+v", c.Status())
	return Status{}
}

func TestConnectLoginAndStatus(t *testing.T) {
	f := newFake(t, false)
	c, err := New(testConfig(f))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	st := waitFor(t, c, func(s Status) bool { return s.Connected })
	if st.Version != "TrueNAS-TEST-25.10.5" {
		t.Errorf("version = %q, want the fake's", st.Version)
	}
	if _, violations, logins := f.count(); len(violations) != 0 {
		t.Errorf("params contract violated: %v", violations)
	} else if len(logins) == 0 {
		t.Error("no login reached the fake")
	} else if logins[0]["mechanism"] != "API_KEY_PLAIN" || logins[0]["username"] != "pademelon" {
		t.Errorf("login params = %v", logins[0])
	}

	// A direct call rides the maintained connection.
	res, err := c.Call(context.Background(), "system.version")
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	var v string
	if json.Unmarshal(res, &v) != nil || v != "TrueNAS-TEST-25.10.5" {
		t.Errorf("Call result = %s", res)
	}
}

func TestCallAlwaysSendsParamsArray(t *testing.T) {
	// The recon trap, pinned: a Go nil slice marshals to null and the
	// server rejects it. Call with no params must still send [].
	f := newFake(t, false)
	c, _ := New(testConfig(f))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)
	waitFor(t, c, func(s Status) bool { return s.Connected })

	if _, err := c.Call(context.Background(), "core.ping"); err != nil {
		t.Fatalf("zero-param call: %v", err)
	}
	if _, violations, _ := f.count(); len(violations) != 0 {
		t.Errorf("params contract violated: %v", violations)
	}
}

func TestSkipsUnsolicitedMessages(t *testing.T) {
	f := newFake(t, false)
	f.notifications = 3
	c, _ := New(testConfig(f))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	st := waitFor(t, c, func(s Status) bool { return s.Connected })
	if st.Version != "TrueNAS-TEST-25.10.5" {
		t.Errorf("version = %q — the unsolicited messages broke the reply match", st.Version)
	}
}

func TestCallTimesOut(t *testing.T) {
	// A stuck middleware must cost the caller one bounded wait, not a
	// hang — the whole reason actions became jobs in phase 2.
	f := newFake(t, false)
	f.delay = 2 * time.Second
	c, _ := New(testConfig(f))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)
	waitFor(t, c, func(s Status) bool { return s.Connected })

	start := time.Now()
	_, err := c.Call(context.Background(), "system.version")
	if err == nil {
		t.Fatal("call against a silent middleware succeeded")
	}
	if time.Since(start) > time.Second {
		t.Errorf("call took %s, the configured bound should have cut it sooner", time.Since(start))
	}
}

func TestReconnectsAfterDrop(t *testing.T) {
	// The NAS reboots; the client must notice on the next keepalive,
	// redial and come back — without a nudge from anyone.
	f := newFake(t, false)
	f.closeAfterPing = true
	c, _ := New(testConfig(f))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	waitFor(t, c, func(s Status) bool { return s.Connected })
	// The keepalive drop plus reconnect is fast; wait for a second accept.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if conns, _, _ := f.count(); conns >= 2 {
			waitFor(t, c, func(s Status) bool { return s.Connected })
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("client never reconnected after the connection was dropped")
}

func TestLoginFailureRecordsError(t *testing.T) {
	f := newFake(t, false)
	f.failLogin = true
	c, _ := New(testConfig(f))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	st := waitFor(t, c, func(s Status) bool { return !s.Connected && s.Error != "" })
	if !strings.Contains(st.Error, "AUTH_ERR") {
		t.Errorf("status error = %q, want the response_type in there", st.Error)
	}
}

func TestTLSWithSkipVerify(t *testing.T) {
	// The production path: wss against a self-signed cert. httptest's TLS
	// server is exactly that, so the default (skip-verify) config must
	// connect to it.
	f := newFake(t, true)
	c, _ := New(testConfig(f))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)
	waitFor(t, c, func(s Status) bool { return s.Connected })
}

func TestNewRejectsBadCAFile(t *testing.T) {
	cfg := testConfig(newFake(t, false))
	cfg.CAFile = "/nonexistent/ca.pem"
	if _, err := New(cfg); err == nil {
		t.Fatal("New accepted an unreadable CA file; it must be a config error")
	}
}

func TestDialURL(t *testing.T) {
	tests := []struct{ host, want string }{
		{"192.168.1.100", "wss://192.168.1.100:443/api/current"},
		{"nas.local:444", "wss://nas.local:444/api/current"},
		{"ws://127.0.0.1:1234/api/current", "ws://127.0.0.1:1234/api/current"},
	}
	for _, tc := range tests {
		if got := dialURL(tc.host); got != tc.want {
			t.Errorf("dialURL(%q) = %q, want %q", tc.host, got, tc.want)
		}
	}
}

func TestDatasetFromDiskSource(t *testing.T) {
	tests := []struct {
		source string
		want   string
		ok     bool
	}{
		{source: "/dev/zvol/nvme/vms/alpine_test-bxuwle", want: "nvme/vms/alpine_test-bxuwle", ok: true},
		{source: "/dev/zvol/tank", want: "tank", ok: true},
		{source: "/mnt/pool/disks/file.qcow2", want: "", ok: false},
		{source: "/dev/zvol/", want: "", ok: false},
		{source: "", want: "", ok: false},
	}
	for _, tc := range tests {
		got, ok := DatasetFromDiskSource(tc.source)
		if got != tc.want || ok != tc.ok {
			t.Errorf("DatasetFromDiskSource(%q) = %q, %v; want %q, %v", tc.source, got, ok, tc.want, tc.ok)
		}
	}
}

func TestSnapshotsQueryAndParsing(t *testing.T) {
	// The live recon shapes, canned: one filtered call per dataset, ZFS
	// properties riding along in rawvalue, and nothing else needed.
	f := newFake(t, false)
	f.snapshots = map[string][]Snapshot{
		"nvme/vms/alpine_test-bxuwle": {
			{ID: "nvme/vms/alpine_test-bxuwle@pademelon-alpine_test-2026-09-07_13-08", Dataset: "nvme/vms/alpine_test-bxuwle", Name: "pademelon-alpine_test-2026-09-07_13-08", Created: 1788749817, Used: 225280, Referenced: 122425344},
			{ID: "nvme/vms/alpine_test-bxuwle@auto-2026-09-07_12-30", Dataset: "nvme/vms/alpine_test-bxuwle", Name: "auto-2026-09-07_12-30", Created: 1788748200, Used: 112640, Referenced: 122425344},
		},
	}
	c, _ := New(testConfig(f))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)
	waitFor(t, c, func(s Status) bool { return s.Connected })

	list, err := c.Snapshots(ctx, "nvme/vms/alpine_test-bxuwle")
	if err != nil {
		t.Fatalf("Snapshots: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("got %d snapshots, want 2", len(list))
	}
	if list[0].Name != "pademelon-alpine_test-2026-09-07_13-08" || list[0].Created != 1788749817 || list[0].Used != 225280 {
		t.Errorf("snapshot parse = %+v", list[0])
	}

	// A dataset with no snapshots: empty list, not an error.
	empty, err := c.Snapshots(ctx, "nvme/vms/nothing-here")
	if err != nil || len(empty) != 0 {
		t.Errorf("empty dataset = %v, %v; want empty, no error", empty, err)
	}
}

func TestCreateSnapshot(t *testing.T) {
	// The middleware answers pool.snapshot.create with the serialized
	// snapshot; its id is the handle the job reports.
	f := newFake(t, false)
	f.created = map[string]string{} // dataset@name -> echo
	c, _ := New(testConfig(f))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)
	waitFor(t, c, func(s Status) bool { return s.Connected })

	id, err := c.CreateSnapshot(ctx, "nvme/vms/alpine_test-bxuwle", "pademelon-alpine_test-2026-09-07_14-32-05")
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	want := "nvme/vms/alpine_test-bxuwle@pademelon-alpine_test-2026-09-07_14-32-05"
	if id != want {
		t.Errorf("id = %q, want %q", id, want)
	}
}

// TestConnectionSurvivesSlowReply is the regression test for the live
// failure of 2026-09-07 ("use of closed network connection"): a reply
// slower than the call bound used to stop the read and kill the
// connection. Now the timeout bounds the wait only — the next call on
// the same connection works, and the late reply is skipped harmlessly.
func TestConnectionSurvivesSlowReply(t *testing.T) {
	f := newFake(t, false)
	f.delay = 2 * time.Second // slower than the 500ms call bound
	c, _ := New(testConfig(f))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)
	waitFor(t, c, func(s Status) bool { return s.Connected })

	// The handshake's own version call was the fast first one; this one
	// hits the delay and times out.
	if _, err := c.Call(context.Background(), "system.version"); err == nil {
		t.Fatal("a slower-than-bound reply should time the call out")
	}

	// The connection must survive the timeout. Clear the delay and keep
	// calling: the fake finishes its one long sleep, after which replies
	// are instant. The next call in the retry then rides the same
	// session.
	f.mu.Lock()
	f.delay = 0
	f.mu.Unlock()
	deadline := time.Now().Add(3 * time.Second)
	for {
		res, err := c.Call(context.Background(), "system.version")
		if err == nil {
			if !strings.Contains(string(res), "TrueNAS-TEST") {
				t.Errorf("unexpected result: %s", res)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("call after a timed-out call: %v — the connection did not survive", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestPendingFailedWhenConnectionDies: a call in flight when the socket
// dies gets "connection lost" promptly — no caller waits out its full
// timeout against a dead connection.
func TestPendingFailedWhenConnectionDies(t *testing.T) {
	f := newFake(t, false)
	f.delay = 2 * time.Second
	c, _ := New(testConfig(f))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)
	waitFor(t, c, func(s Status) bool { return s.Connected })

	errCh := make(chan error, 1)
	go func() {
		_, err := c.Call(context.Background(), "system.version")
		errCh <- err
	}()

	// The call is now in flight against a stalling fake. Kill the socket
	// the way a NAS restart would.
	time.Sleep(50 * time.Millisecond)
	f.dropConnections()
	// The stall's job is done — the in-flight call learns "connection
	// lost" from the client's own read-loop death, never from this fake's
	// reply. Without resetting it, every reconnect's version proof stalls
	// too, and the client could never come back within the test's wait.
	f.mu.Lock()
	f.delay = 0
	f.mu.Unlock()

	select {
	case err := <-errCh:
		if !strings.Contains(err.Error(), "connection lost") {
			t.Errorf("in-flight call error = %v, want the connection-lost reason", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight call never learned the connection died")
	}

	// The client recovers on its own.
	waitFor(t, c, func(s Status) bool { return s.Connected })
}

// TestWakeInterruptsReconnectBackoff: a client dialling an unreachable
// NAS retries on the backoff. A wake pulls the next attempt forward —
// visible as a second recorded failure long before the backoff would
// have elapsed. This is what makes an on-demand snapshot fetch able to
// trigger the reconnect instead of waiting it out.
func TestWakeInterruptsReconnectBackoff(t *testing.T) {
	c, err := New(Config{
		Host:     "wss://127.0.0.1:1/api/current", // nothing listens: instant refusal
		Username: "pademelon",
		APIKey:   "test-key",
		Timeout:  200 * time.Millisecond,
		Backoff:  50 * time.Second, // without a wake, the second attempt is 50s away
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	// The first failed attempt records itself; waitFor would time out at
	// 3s if the wake did not pull the second attempt forward.
	first := waitFor(t, c, func(s Status) bool { return !s.Connected && s.Error != "" })
	c.Wake()
	waitFor(t, c, func(s Status) bool { return s.Since.After(first.Since) && s.Error != "" })
}

// TestWakeDuringHealthyConnectionIsHarmless: the wake channel is a hint,
// never a disruption — a healthy connection ignores it and stays up.
func TestWakeDuringHealthyConnectionIsHarmless(t *testing.T) {
	f := newFake(t, false)
	c, _ := New(testConfig(f))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)
	waitFor(t, c, func(s Status) bool { return s.Connected })

	c.Wake()
	c.Wake()
	c.Wake()

	// The connection answers calls as if nothing happened.
	waitFor(t, c, func(s Status) bool { return s.Connected })
	if _, err := c.Call(context.Background(), "core.ping"); err != nil {
		t.Errorf("call after wakes: %v — the wake must not disturb a healthy connection", err)
	}
}
