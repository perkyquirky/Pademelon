// Package truenas speaks the TrueNAS middleware's websocket API: JSON-RPC
// 2.0 over wss, the only supported surface since the removal of the REST
// API (IDEAS-EXPLORED.md §8). Everything here is read-only in spirit —
// Stage 1 wires up connection, login and status; later stages add the
// snapshot calls, which live behind internal/actions like every other
// write.
//
// Shapes settled by live recon against 25.10.5, and enforced by tests:
//
//   - params is ALWAYS an array, even for zero-argument methods. Go's nil
//     slice marshals to null and the server rejects that with -32600.
//   - auth.login_ex with mechanism "API_KEY_PLAIN" takes username + api_key
//     and answers {"response_type": "SUCCESS", ...}.
//   - the server may send unsolicited notifications at any time; replies
//     are matched by id and everything else is skipped, never fatal.
//   - a refused call comes back as -32001 with errno 13 (EACCES) — the
//     service account's privilege is the scope, so the client surfaces the
//     error verbatim rather than rewording it.
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

	// Timeout, Keepalive and Backoff override the clocks constants when
	// positive. Injectable so tests do not wait on real seconds.
	Timeout   time.Duration
	Keepalive time.Duration
	Backoff   time.Duration

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

// Client owns the one middleware connection. The background Run loop dials,
// logs in and keeps the connection proven; Call rides that connection. A
// single in-flight request at a time (the round-trip mutex) is plenty at
// homelab scale and keeps reply-matching trivially correct.
type Client struct {
	cfg Config
	log *slog.Logger
	tls *tls.Config

	// mu guards the connection and the id counter, and is held for the
	// whole of a request/response round trip.
	mu     sync.Mutex
	conn   *websocket.Conn
	nextID int

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

	return &Client{cfg: cfg, log: cfg.Log, tls: tlsCfg}, nil
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
// pings, and a reconnect loop with a pause between attempts for the times
// the NAS is down or rebooting. It is the only writer of the connection;
// Call borrows it.
func (c *Client) Run(ctx context.Context) {
	keepalive := c.cfg.Keepalive
	if keepalive <= 0 {
		keepalive = clocks.MiddlewareKeepalive
	}
	backoff := c.cfg.Backoff
	if backoff <= 0 {
		backoff = clocks.MiddlewareReconnectBackoff
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
			}
			continue
		}

		select {
		case <-ctx.Done():
			c.mu.Lock()
			conn := c.conn
			c.conn = nil
			c.mu.Unlock()
			if conn != nil {
				_ = conn.Close(websocket.StatusNormalClosure, "shutdown")
			}
			return
		case <-ticker.C:
			pctx, cancel := context.WithTimeout(ctx, c.timeout())
			_, err := c.Call(pctx, "core.ping")
			cancel()
			if err != nil && ctx.Err() == nil {
				c.log.Warn("middleware keepalive failed; reconnecting", "err", err)
				c.drop(err)
			}
		}
	}
}

// ensureConnected dials, logs in and fills in the version — or records the
// failure in the status for the UI to show.
func (c *Client) ensureConnected(ctx context.Context) error {
	c.mu.Lock()
	if c.conn != nil {
		c.mu.Unlock()
		return nil
	}
	c.mu.Unlock()

	conn, err := c.dial(ctx)
	if err != nil {
		c.setState(false, "", err.Error())
		return err
	}
	if err := c.handshake(ctx, conn); err != nil {
		conn.CloseNow()
		c.setState(false, "", err.Error())
		return err
	}

	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()
	c.log.Info("connected to TrueNAS middleware", "host", c.cfg.Host)

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

// drop tears the connection down and records why. Nothing here retries —
// Run's loop is the retry.
func (c *Client) drop(err error) {
	c.mu.Lock()
	conn := c.conn
	c.conn = nil
	c.mu.Unlock()
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

// handshake logs the connection in. It runs before the connection is
// stored, so it uses roundTrip directly rather than Call.
func (c *Client) handshake(ctx context.Context, conn *websocket.Conn) error {
	hctx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()
	res, err := c.roundTrip(hctx, conn, "auth.login_ex", map[string]any{
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

// Call sends one request on the maintained connection and waits for the
// matching reply. Concurrency model: one round trip at a time — middleware
// calls are sub-second and Pademelon's callers are few, so the simple
// lock is the better choice over a pending-request map in both code and
// correctness.
func (c *Client) Call(ctx context.Context, method string, params ...any) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return nil, ErrNotConnected
	}
	cctx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()
	// The spread matters: forwarding params as a bare argument would wrap
	// the slice in another array — the live server answered "Too many
	// arguments" to exactly that before a test caught the shape.
	return c.roundTrip(cctx, c.conn, method, params...)
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

// roundTrip writes one request and reads until the reply with its id
// arrives. Unsolicited messages (job progress, subscription pushes) are
// skipped with a debug line — losing one never breaks a pending call.
func (c *Client) roundTrip(ctx context.Context, conn *websocket.Conn, method string, params ...any) (json.RawMessage, error) {
	if params == nil {
		params = []any{} // nil marshals to null; the server demands an array
	}
	c.nextID++
	id, _ := json.Marshal(c.nextID)
	req, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      json.RawMessage(id),
		"method":  method,
		"params":  params,
	})

	if err := conn.Write(ctx, websocket.MessageText, req); err != nil {
		return nil, fmt.Errorf("write %s: %w", method, err)
	}
	want := string(id)
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", method, err)
		}
		var r rpcResponse
		if err := json.Unmarshal(data, &r); err != nil {
			return nil, fmt.Errorf("decode %s reply: %w (%.120s)", method, err, data)
		}
		if string(r.ID) != want {
			c.log.Debug("skipping unsolicited middleware message", "while_waiting_for", method, "payload", string(data))
			continue
		}
		if r.Error != nil {
			return nil, r.Error
		}
		return r.Result, nil
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
