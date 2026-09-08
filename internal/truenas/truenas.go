// Package truenas speaks the TrueNAS middleware's websocket API: JSON-RPC
// 2.0 over wss, the only supported surface since the removal of the REST
// API (IDEAS-EXPLORED.md §8). Everything here is read-only in spirit —
// the snapshot calls are typed methods; the write-side orchestration
// lives behind internal/actions like every other write.
//
// Shapes settled by live recon against 25.10.5, and enforced by tests:
//
//   - params is ALWAYS an array, even for zero-argument methods. Go's nil
//     slice marshals to null and the server rejects that with -32600.
//   - auth.login_ex with mechanism "API_KEY_PLAIN" takes username + api_key
//     and answers {"response_type": "SUCCESS", ...}.
//   - the server may send unsolicited notifications at any time; the read
//     loop consumes them, never fatally.
//   - a refused call comes back as -32001 with errno 13 (EACCES) — the
//     service account's privilege is the scope, so the client surfaces the
//     error verbatim rather than rewording it.
//
// Connection design, from a failure seen in live use: one reader
// goroutine owns all reads for the life of the connection. It answers
// control frames (the server closes a session with no reader),
// consumes unsolicited messages, and dispatches replies to waiting calls
// by id. Call timeouts bound the wait for a reply, never the read — a
// slow reply costs one timed-out call, not the connection.
package truenas

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"pademelon/internal/clocks"
)

// apiPath is the websocket endpoint. The legacy /websocket path speaks the
// pre-JSON-RPC protocol; /api/current is the current one.
const apiPath = "/api/current"

// ErrNotConnected is returned by Call when the background loop has no live
// connection yet — middleware down, NAS rebooting, or Run not started.
var ErrNotConnected = errors.New("middleware not connected")

// Config is what New needs. Zero-value durations fall back to the clocks
// constants, the same pattern the actions store uses.
type Config struct {
	// Host is the NAS address, "nas.local" or "nas.local:444". A value
	// containing a scheme is used as-is — that is the test seam for plain
	// ws:// fakes; production always builds wss://.
	Host string

	// Username is the service account that owns the API key
	// (auth.login_ex wants the pair).
	Username string

	// APIKey is the service account's key. Never logged.
	APIKey string

	// CAFile is an optional CA bundle to verify the middleware's
	// certificate. Empty means skip-verify — the documented trade for a
	// NAS shipping a self-signed cert (IDEAS-EXPLORED.md §8.2).
	CAFile string

	// Timeout, Keepalive, Backoff and RetryFloor override the clocks
	// constants when positive. Injectable so tests do not wait on real
	// seconds.
	Timeout    time.Duration
	Keepalive  time.Duration
	Backoff    time.Duration
	RetryFloor time.Duration

	Log *slog.Logger
}

// Status is the human-readable connection state: what the status line and
// /api/truenas render. Version survives a disconnect — the last known
// release is more useful than a blank.
type Status struct {
	Connected bool      `json:"connected"`
	Version   string    `json:"version,omitempty"`
	Error     string    `json:"error,omitempty"`
	Since     time.Time `json:"since"`
}

// callResult is what the read loop hands a waiting call.
type callResult struct {
	data json.RawMessage
	err  error
}

// Client owns the one middleware connection. The background Run loop dials,
// logs in and keeps the connection proven; Call rides that connection.
// Writes are serialized (one writer at a time on a websocket); reads belong
// exclusively to the per-connection read loop.
type Client struct {
	cfg Config
	log *slog.Logger
	tls *tls.Config

	connMu       sync.Mutex
	conn         *websocket.Conn
	readerCancel context.CancelFunc
	pending      map[int64]chan callResult
	nextID       int64

	writeMu sync.Mutex // one writer at a time on the socket

	// dead carries one signal per lost connection; Run waits on it.
	dead chan struct{}

	// wake interrupts a reconnect backoff so the next dial attempt
	// starts now. One slot, non-blocking: a wake that arrives while the
	// connection is healthy is dropped, not queued.
	wake chan struct{}

	stateMu sync.RWMutex
	state   Status
}

// New validates config that can be checked without dialling — a CAFile
// that cannot be read is a config error and should refuse startup, not
// wait until the first connect. It does not dial; Run does that.
func New(cfg Config) (*Client, error) {
	if cfg.Host == "" {
		return nil, fmt.Errorf("middleware host is empty")
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}

	var tlsCfg *tls.Config
	if cfg.CAFile == "" {
		// The documented trade (README2, "TrueNAS middleware"): the NAS
		// ships a self-signed certificate; credential verification still
		// happens, certificate identity does not.
		tlsCfg = &tls.Config{InsecureSkipVerify: true}
	} else {
		pem, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("reading CA file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("CA file %s contains no certificates", cfg.CAFile)
		}
		tlsCfg = &tls.Config{RootCAs: pool}
	}

	return &Client{
		cfg:     cfg,
		log:     cfg.Log,
		tls:     tlsCfg,
		pending: map[int64]chan callResult{},
		dead:    make(chan struct{}, 1),
		wake:    make(chan struct{}, 1),
	}, nil
}

// dialURL turns the configured host into the websocket address.
func dialURL(host string) string {
	if strings.Contains(host, "://") {
		return host // test seam: a full ws:// or wss:// URL
	}
	if !strings.Contains(host, ":") {
		host += ":443"
	}
	return "wss://" + host + apiPath
}

func (c *Client) timeout() time.Duration {
	if c.cfg.Timeout > 0 {
		return c.cfg.Timeout
	}
	return clocks.MiddlewareTimeout
}

// Status is a snapshot of the connection state for readers.
func (c *Client) Status() Status {
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	return c.state
}

// Wake asks the Run loop to stop waiting out a reconnect backoff and dial
// now. It is a hint for on-demand callers (a snapshot fetch after a
// dropout): when the connection is healthy it does nothing, and a wake
// that lands during a healthy stretch is consumed as a harmless no-op.
func (c *Client) Wake() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// setState records a transition. Version is kept across failures — the
// last known release is better than a blank when the NAS is mid-reboot.
func (c *Client) setState(connected bool, version, errMsg string) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	if version == "" {
		version = c.state.Version
	}
	c.state = Status{Connected: connected, Version: version, Error: errMsg, Since: time.Now()}
}

// Run maintains the connection until ctx is done: dial, login, keepalive
// pings, and a reconnect loop with a pause between attempts for the
// times the NAS is down or rebooting. It owns the connection lifecycle;
// Call borrows the connection, and the read loop owns its reads.
func (c *Client) Run(ctx context.Context) {
	keepalive := c.cfg.Keepalive
	if keepalive <= 0 {
		keepalive = clocks.MiddlewareKeepalive
	}
	backoff := c.cfg.Backoff
	if backoff <= 0 {
		backoff = clocks.MiddlewareReconnectBackoff
	}
	retryFloor := c.cfg.RetryFloor
	if retryFloor <= 0 {
		retryFloor = clocks.MiddlewareRetryFloor
	}

	ticker := time.NewTicker(keepalive)
	defer ticker.Stop()

	for {
		if err := c.ensureConnected(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			c.log.Warn("middleware connect failed; backing off",
				"host", c.cfg.Host, "retry_in", backoff, "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			case <-c.wake:
				// An on-demand caller wants the list now; dial
				// immediately instead of finishing the backoff.
				c.log.Info("middleware reconnect pulled forward by a wake")
			}
			continue
		}

		select {
		case <-ctx.Done():
			c.drop(errors.New("client shut down"))
			return
		case <-c.dead:
			// The read loop lost the connection. Tear the dead
			// connection down so nothing writes to it again, then
			// recover quickly — a transient close should cost seconds,
			// not the full backoff — but keep the floor so a server
			// that closes connections immediately cannot drive a
			// rapid retry loop.
			c.drop(errors.New("connection lost"))
			c.log.Info("middleware connection lost; reconnecting")
			select {
			case <-ctx.Done():
				return
			case <-time.After(retryFloor):
			case <-c.wake:
				c.log.Info("middleware reconnect pulled forward by a wake")
			}
		case <-c.wake:
			// A wake during a healthy connection: nothing to do, the
			// next keepalive proves the socket on its own schedule.
		case <-ticker.C:
			pctx, cancel := context.WithTimeout(ctx, c.timeout())
			_, err := c.Call(pctx, "core.ping")
			cancel()
			if err != nil && ctx.Err() == nil {
				// Only dead-socket evidence drops the connection. A
				// timeout with the read loop still alive means a busy
				// server, not a dead socket — dropping then would tear
				// down healthy in-flight traffic. If the socket is
				// really gone, the read loop reports it and the dead
				// channel fires.
				if isDeadSocketErr(err) {
					c.log.Warn("middleware keepalive failed; reconnecting", "err", err)
					c.drop(err)
				} else {
					c.log.Warn("middleware keepalive timed out; retrying", "err", err)
				}
			}
		}
	}
}

// ensureConnected dials, logs in and fills in the version — or records the
// failure in the status for the UI to show.
func (c *Client) ensureConnected(ctx context.Context) error {
	c.connMu.Lock()
	if c.conn != nil {
		c.connMu.Unlock()
		return nil
	}
	c.connMu.Unlock()

	conn, err := c.dial(ctx)
	if err != nil {
		c.setState(false, "", err.Error())
		return err
	}

	// The read loop starts before the login: it delivers the login's
	// reply. Reads never carry a deadline — a per-call timeout must not
	// corrupt the framing — so the loop lives on a connection-scoped
	// context that drop() cancels.
	rctx, cancel := context.WithCancel(ctx)
	c.connMu.Lock()
	c.readerCancel = cancel
	c.connMu.Unlock()
	go c.readLoop(rctx, conn)

	if err := c.login(ctx, conn); err != nil {
		c.drop(err)
		return err
	}

	c.connMu.Lock()
	c.conn = conn
	c.connMu.Unlock()
	c.log.Info("connected to TrueNAS middleware", "host", c.cfg.Host)

	// A stale loss signal from a previous connection must not delay the
	// new one.
	select {
	case <-c.dead:
	default:
	}

	// The version round-trip doubles as the first proof the session is
	// really good; a failure here drops back into the retry loop.
	vctx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()
	res, err := c.Call(vctx, "system.version")
	if err != nil {
		c.drop(err)
		return err
	}
	var version string
	_ = json.Unmarshal(res, &version)
	c.setState(true, version, "")
	c.log.Info("TrueNAS middleware ready", "version", version)
	return nil
}

// login authenticates one fresh connection: the key check happens here,
// the certificate check in dial.
func (c *Client) login(ctx context.Context, conn *websocket.Conn) error {
	lctx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()
	res, err := c.request(lctx, conn, "auth.login_ex", map[string]any{
		"mechanism": "API_KEY_PLAIN",
		"username":  c.cfg.Username,
		"api_key":   c.cfg.APIKey,
	})
	if err != nil {
		return fmt.Errorf("login: %w", err)
	}
	var auth struct {
		ResponseType string `json:"response_type"`
	}
	if err := json.Unmarshal(res, &auth); err != nil {
		return fmt.Errorf("login: decode result %.120s: %w", res, err)
	}
	if auth.ResponseType != "SUCCESS" {
		return fmt.Errorf("login: response_type %q", auth.ResponseType)
	}
	return nil
}

// drop tears the connection down and records why. The read loop dies with
// the socket; anything waiting for a reply gets the reason. Nothing here
// retries — Run's loop is the retry.
func (c *Client) drop(err error) {
	c.connMu.Lock()
	conn := c.conn
	c.conn = nil
	cancel := c.readerCancel
	c.readerCancel = nil
	c.failPendingLocked(err)
	c.connMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if conn != nil {
		conn.CloseNow()
	}
	c.setState(false, "", err.Error())
}

func (c *Client) dial(ctx context.Context) (*websocket.Conn, error) {
	dctx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()
	conn, _, err := websocket.Dial(dctx, dialURL(c.cfg.Host), &websocket.DialOptions{
		HTTPClient: &http.Client{Transport: &http.Transport{TLSClientConfig: c.tls}},
	})
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", c.cfg.Host, err)
	}
	// Reply bodies are small, but a snapshot query on a dataset with years
	// of auto-* snapshots is a few hundred KB; leave generous headroom.
	conn.SetReadLimit(8 << 20)
	return conn, nil
}

// Call sends one request on the maintained connection and waits for the
// matching reply. The timeout bounds the wait; the read loop continues
// regardless, so a slow reply costs one failed call and never the
// connection.
func (c *Client) Call(ctx context.Context, method string, params ...any) (json.RawMessage, error) {
	c.connMu.Lock()
	conn := c.conn
	c.connMu.Unlock()
	if conn == nil {
		return nil, ErrNotConnected
	}
	return c.request(ctx, conn, method, params...)
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string {
	// The live server wraps most failures in a data object whose "reason"
	// is the readable part; the trace block is debug-grade noise and must
	// not end up in a status line or a log at info.
	var d struct {
		Errname string `json:"errname"`
		Reason  string `json:"reason"`
	}
	if json.Unmarshal(e.Data, &d) == nil && d.Reason != "" {
		if d.Errname != "" && !strings.Contains(d.Reason, d.Errname) {
			return d.Errname + " " + d.Reason
		}
		return d.Reason
	}
	if len(e.Data) > 0 {
		return fmt.Sprintf("%d %s | %.200s", e.Code, e.Message, e.Data)
	}
	return fmt.Sprintf("%d %s", e.Code, e.Message)
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
}

// request writes one request on the given connection and waits for the
// reply the read loop delivers. Pending calls are registered under the
// connection lock and dispatched by numeric id.
func (c *Client) request(ctx context.Context, conn *websocket.Conn, method string, params ...any) (json.RawMessage, error) {
	if params == nil {
		params = []any{} // nil marshals to null; the server demands an array
	}
	c.connMu.Lock()
	c.nextID++
	id := c.nextID
	wait := make(chan callResult, 1)
	c.pending[id] = wait
	c.connMu.Unlock()
	defer c.removePending(id)

	req, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	})

	cctx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()

	c.writeMu.Lock()
	err := conn.Write(cctx, websocket.MessageText, req)
	c.writeMu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("write %s: %w", method, err)
	}

	select {
	case res := <-wait:
		if res.err != nil {
			return nil, fmt.Errorf("%s: %w", method, res.err)
		}
		return res.data, nil
	case <-cctx.Done():
		return nil, fmt.Errorf("%s: %w", method, cctx.Err())
	}
}

// readLoop owns the connection's reads until the context is cancelled or
// the socket dies. It answers control frames (the server closes sessions
// that never read), consumes unsolicited messages, and dispatches replies
// to waiting calls by id.
func (c *Client) readLoop(ctx context.Context, conn *websocket.Conn) {
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			c.failPending(fmt.Errorf("connection lost: %w", err))
			select {
			case c.dead <- struct{}{}:
			default:
			}
			return
		}
		var r rpcResponse
		if json.Unmarshal(data, &r) != nil {
			c.log.Debug("middleware sent undecodable message", "payload", string(data))
			continue
		}
		if len(r.ID) == 0 {
			c.log.Debug("middleware notification", "payload", string(data))
			continue
		}
		var id int64
		if json.Unmarshal(r.ID, &id) != nil {
			c.log.Debug("middleware reply with unreadable id", "payload", string(data))
			continue
		}
		c.connMu.Lock()
		wait, ok := c.pending[id]
		if ok {
			delete(c.pending, id)
		}
		c.connMu.Unlock()
		if !ok {
			// A reply for a call that already timed out: the connection
			// is healthy, the caller just stopped waiting.
			c.log.Debug("late middleware reply, no caller waiting", "id", id)
			continue
		}
		if r.Error != nil {
			wait <- callResult{err: r.Error}
		} else {
			wait <- callResult{data: r.Result}
		}
	}
}

// removePending forgets a call whose caller stopped waiting (timeout,
// cancelled context). The read loop skips its late reply when it arrives.
func (c *Client) removePending(id int64) {
	c.connMu.Lock()
	delete(c.pending, id)
	c.connMu.Unlock()
}

// failPending hands every waiting call the connection's reason. Called
// when the read loop dies or the connection is dropped.
func (c *Client) failPending(err error) {
	c.connMu.Lock()
	c.failPendingLocked(err)
	c.connMu.Unlock()
}

func (c *Client) failPendingLocked(err error) {
	for id, wait := range c.pending {
		delete(c.pending, id)
		wait <- callResult{err: err}
	}
}

// Snapshot is one zvol snapshot as the middleware reports it — the fields
// the dashboard shows, nothing more. Created is epoch seconds; Used is the
// copy-on-write divergence (usually tiny); Referenced is the size the
// snapshot restores to.
type Snapshot struct {
	ID         string
	Dataset    string
	Name       string // the snapshot name alone, e.g. pademelon-alpine_test-2026-09-07_13-08
	Created    int64
	Used       uint64
	Referenced uint64
}

// zfsProp is the shape of one ZFS property in a snapshot query reply.
// The value humans read lives in rawvalue; parsed carries typed variants
// the dashboard does not need.
type zfsProp struct {
	Rawvalue string `json:"rawvalue"`
}

// Snapshots lists one dataset's snapshots with a server-side filter —
// one filtered call per dataset, never one unfiltered pool-wide query.
func (c *Client) Snapshots(ctx context.Context, dataset string) ([]Snapshot, error) {
	res, err := c.Call(ctx, "pool.snapshot.query", [][]any{{"dataset", "=", dataset}}, map[string]any{})
	if err != nil {
		return nil, err
	}
	var items []struct {
		ID           string             `json:"id"`
		Dataset      string             `json:"dataset"`
		SnapshotName string             `json:"snapshot_name"`
		Properties   map[string]zfsProp `json:"properties"`
	}
	if err := json.Unmarshal(res, &items); err != nil {
		return nil, fmt.Errorf("decode snapshot query for %s: %w", dataset, err)
	}
	out := make([]Snapshot, 0, len(items))
	for _, it := range items {
		s := Snapshot{ID: it.ID, Dataset: it.Dataset, Name: it.SnapshotName}
		// ZFS reports creation as an epoch-seconds string and space as a
		// plain byte count — both in rawvalue, both fine to zero when a
		// property is missing.
		if p, ok := it.Properties["creation"]; ok {
			s.Created, _ = strconv.ParseInt(p.Rawvalue, 10, 64)
		}
		if p, ok := it.Properties["used"]; ok {
			s.Used, _ = strconv.ParseUint(p.Rawvalue, 10, 64)
		}
		if p, ok := it.Properties["referenced"]; ok {
			s.Referenced, _ = strconv.ParseUint(p.Rawvalue, 10, 64)
		}
		out = append(out, s)
	}
	return out, nil
}

// CreateSnapshot takes one snapshot on one dataset. The middleware call is
// fast (copy-on-write); the caller owns the sequencing — this method
// deliberately knows nothing about freezing guests. The returned id is the
// full "dataset@name" handle.
func (c *Client) CreateSnapshot(ctx context.Context, dataset, name string) (string, error) {
	res, err := c.Call(ctx, "pool.snapshot.create", map[string]any{
		"dataset": dataset,
		"name":    name,
	})
	if err != nil {
		return "", err
	}
	var r struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(res, &r); err != nil {
		return "", fmt.Errorf("decode create result: %w (%.120s)", err, res)
	}
	if r.ID == "" {
		return "", fmt.Errorf("create succeeded but the middleware sent no snapshot id")
	}
	return r.ID, nil
}

// RollbackSnapshot rewinds one dataset to one snapshot. When recursive is
// true the middleware destroys any newer snapshots on the way — that flag
// is the caller's acknowledgement of exactly that, never a silent default
// (IDEAS-EXPLORED.md §8.6).
func (c *Client) RollbackSnapshot(ctx context.Context, dataset, name string, recursive bool) error {
	opts := map[string]any{}
	if recursive {
		opts["recursive"] = true
	}
	_, err := c.Call(ctx, "pool.snapshot.rollback", dataset+"@"+name, opts)
	if err != nil {
		return fmt.Errorf("rollback %s@%s: %w", dataset, name, err)
	}
	return nil
}

// DeleteSnapshot removes one snapshot. A snapshot that is already gone is
// not an error — the bool says whether this call actually deleted
// something, and false with nil error means "already gone".
func (c *Client) DeleteSnapshot(ctx context.Context, dataset, name string) (bool, error) {
	id := dataset + "@" + name
	_, err := c.Call(ctx, "pool.snapshot.delete", id)
	if err == nil {
		return true, nil
	}
	if isNotFoundErr(err) {
		return false, nil
	}
	return false, fmt.Errorf("delete %s: %w", id, err)
}

// isNotFoundErr recognises the middleware's "no such snapshot" answer:
// a -32602 carrying InstanceNotFound and reason "[ENOENT] ... not found"
// (captured live in recon and pinned in the error-shape tests).
func isNotFoundErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "ENOENT") || strings.Contains(msg, "not found")
}

// isDeadSocketErr separates "the socket is gone" from "the server is
// slow". Only the first drops the connection; a timeout with the read
// loop still alive gets a retry, not a drop.
func isDeadSocketErr(err error) bool {
	if errors.Is(err, ErrNotConnected) {
		return true
	}
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "use of closed network connection") ||
		strings.Contains(msg, "connection lost") ||
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "connection reset by peer")
}

// DatasetFromDiskSource converts a domain XML disk source to the
// middleware's dataset name: "/dev/zvol/pool/vms/x" → "pool/vms/x".
// ok is false for anything that is not a zvol — file-backed disks, ISOs
// and qcow2 images have no zvol to snapshot, and pretending otherwise
// would send the middleware a dataset name that does not exist.
func DatasetFromDiskSource(source string) (string, bool) {
	const prefix = "/dev/zvol/"
	if !strings.HasPrefix(source, prefix) {
		return "", false
	}
	ds := strings.TrimPrefix(source, prefix)
	if ds == "" {
		return "", false
	}
	return ds, true
}
