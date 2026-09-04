# Pademelon — Security Best Practices Report

Reviewed against a Go/`net/http` security spec (server hardening, auth, XSS,
concurrency, supply chain). Context that shapes every severity below:
**Pademelon is a local tool. It is never internet-facing.** Each finding lists
the severity it would carry for an internet-facing service alongside the
severity that actually applies here.

Scope: everything — `cmd/pademelon`, all five `internal/` packages, the
embedded `index.html`, `Dockerfile`, both compose files, and
`.github/workflows/build.yml`.

## Executive summary

The codebase is in good shape, largely because the design removes whole
vulnerability classes instead of patching them: GET-only routes, zero request
body parsing, domain names that never come from the request, and a CI grep
that enforces the read-only rule. Auth is a static token compared in constant
time, the cookie is HttpOnly + SameSite=Lax, and the token is never logged.

No critical findings. Six small hardening items, one benign data race, and two
design decisions documented as accepted risks. The most worthwhile fixes are
HTTP server timeouts (F1), the cache race (F2), and `-race`/`govulncheck` in
CI (F7, F8) — all small, low-regression changes.

## What's already right

Worth recording so future changes don't quietly erode it:

- **Read-only surface.** Every route is GET (`internal/web/web.go:61-68`);
  no handler reads `r.Body`; domain names come only from the poller's libvirt
  listing. CI fails the build on libvirt write calls or `guest-exec`
  (`.github/workflows/build.yml:30-42`), and `guest-exec` is explicitly
  banned in `internal/agent/agent.go:7-8`.
- **Token handling.** Constant-time compare via `crypto/subtle`
  (`internal/web/auth.go:100-102`); the token is never logged — startup logs
  only *where* it came from (`cmd/pademelon/main.go:101-105`); env and
  Docker-secrets file resolution with a fail-closed check on configured-but-
  empty (`cmd/pademelon/main.go:211-230`).
- **Cookie flags.** HttpOnly, SameSite=Lax, 30-day Max-Age, and `Secure` set
  only when the request actually arrived over TLS — the correct conditional,
  not the usual "always Secure" mistake (`internal/web/auth.go:136-149`).
- **Brute-force throttle.** Exponential backoff per source IP with a pruned
  map, so it can't grow unboundedly (`internal/web/auth.go:69-96`).
- **Client-IP discipline.** `remoteIP` uses `r.RemoteAddr` and never trusts
  `X-Forwarded-*` (`internal/web/auth.go:239-245`).
- **Frontend escaping.** `esc()` applied consistently to guest-derived
  strings (hostname, domain, OS, kernel, agent errors, mountpoints) in
  `internal/web/index.html:780-784` and its call sites; the token lives only
  in an HttpOnly cookie, never in JS-readable storage.
- **Server-side JSON escaping.** `encoding/json` HTML-escapes by default, so
  `/api/vms` output is safe to embed (`internal/web/web.go:91-95`).
- **Allowlisted config.** Theme names validated against a fixed list before
  any interpolation into the page (`internal/web/theme.go:23-30`,
  `cmd/pademelon/main.go:60-63`).
- **No debug surface.** No pprof, expvar, CORS, redirects, file serving, or
  shell-outs anywhere in the tree.
- **Supply chain.** Modern Go (1.26.6), a two-dependency module graph,
  committed `go.sum`, no `GOSUMDB`/`GOINSECURE`/`GOPROXY` overrides, digest
  is only the docker build being plain `golang:1.26-alpine`.
- **Outbound calls bounded.** The only HTTP client (the self-probe) has an
  explicit timeout (`cmd/pademelon/main.go:190`), and all libvirt dials use a
  10s timeout (`internal/libvirtsrc/source.go:159-162`).

## Findings

### F1 — HTTP server missing write/idle timeouts and header size cap

- **Rule:** GO-HTTP-001
- **Severity:** Medium (spec: High for internet-facing)
- **Location:** `cmd/pademelon/main.go:123-127`
- **Evidence:**

  ```go
  srv := &http.Server{
      Addr:              *listen,
      Handler:           web.New(cache, log, *theme, token).Handler(),
      ReadHeaderTimeout: clocks.HeaderReadTimeout,
  }
  ```

  `ReadTimeout`, `WriteTimeout`, `IdleTimeout` and `MaxHeaderBytes` are all
  unset (zero = no limit).

- **Impact:** A client on the LAN can open connections and read responses
  arbitrarily slowly, holding a goroutine and file descriptor each. All
  handlers reply from cache so per-request work is cheap — this is a
  resource nuisance, not a crash — but the fix is cheap too.
- **Fix:** Add explicit values to the server literal:

  ```go
  srv := &http.Server{
      Addr:              *listen,
      Handler:           web.New(cache, log, *theme, token).Handler(),
      ReadHeaderTimeout: clocks.HeaderReadTimeout,
      ReadTimeout:       15 * time.Second,
      WriteTimeout:      15 * time.Second,
      IdleTimeout:       60 * time.Second,
      MaxHeaderBytes:    64 << 10,
  }
  ```

  Bounds must stay well above the 3s healthcheck probe timeout
  (`internal/clocks/clocks.go:83`) so `-healthcheck` keeps passing; 15s does
  that with room to spare. `MaxHeaderBytes` of 64 KiB is ample — the largest
  legitimate header is a browser cookie holding a 64-char token. New timers
  belong in `internal/clocks` per the project convention.
- **Regression risk:** Low. Handlers finish in milliseconds; the page's
  1.5s refresh interval sits far inside the idle timeout.
- **False-positive notes:** None — the zero values are definitively unset.

### F2 — Data race: `Cache.SetError` mutates a slice shared with readers

- **Rule:** GO-CONC-001
- **Severity:** Medium (spec: Medium-High)
- **Location:** `internal/model/model.go:160-168`, read path at
  `internal/model/model.go:152-156`
- **Evidence:**

  ```go
  func (c *Cache) SetError(err error) {
      c.mu.Lock()
      defer c.mu.Unlock()
      c.snap.Connected = false
      c.snap.Error = err.Error()
      for i := range c.snap.VMs {
          c.snap.VMs[i].Stale = true   // in-place write to shared backing array
      }
  }
  ```

  `Get()` returns a shallow copy of the `Snapshot` struct — the `VMs` slice
  header is copied, but the backing array is shared with the cache. A handler
  that fetched the snapshot then JSON-encodes it holds no lock while doing
  so; a poll failure landing mid-encode writes `Stale` into the same array
  the encoder is reading.

- **Impact:** In practice the worst outcome is a torn read of a boolean —
  cosmetic. But it is a genuine data race (undefined behaviour in Go, and
  `go test -race` / any race-in-CI plan flags it), and races on shared state
  near a security boundary are worth eliminating on principle.
- **Fix:** Copy-on-write — clone the slice instead of mutating the shared
  one:

  ```go
  func (c *Cache) SetError(err error) {
      c.mu.Lock()
      defer c.mu.Unlock()
      vms := make([]VM, len(c.snap.VMs))
      copy(vms, c.snap.VMs)
      for i := range vms {
          vms[i].Stale = true
      }
      c.snap.VMs = vms
      c.snap.Connected = false
      c.snap.Error = err.Error()
  }
  ```

  Handlers already holding the old array keep reading an array nobody
  writes to anymore.
- **Regression risk:** Low. No code depends on the in-place mutation; check
  `model` package tests still pass (they exercise `SetError`).
- **False-positive notes:** Confirmed by reading both paths; `Set()`
  replaces the snapshot wholesale (fresh slice per poll in
  `internal/libvirtsrc/source.go:188`), so `SetError` is the only in-place
  mutator.

### F3 — Anonymous read tier exposes a detailed guest inventory — *accepted design decision*

- **Rule:** Auth boundary design (no single spec rule)
- **Severity:** Policy / accepted risk (spec: would be High for
  internet-facing)
- **Location:** `internal/web/web.go:61-64` (public routes), `docker-compose.yaml:9-11`
  (`ports: "8088:8088"` — all interfaces)
- **Evidence:** `/api/vms` serves, unauthenticated: VM names and TrueNAS IDs,
  guest hostnames, OS distribution and kernel version, internal IPv4
  addresses, and filesystem mountpoints/usage for every VM — regardless of
  whether a token is configured (the token only gates the private tier).
- **Impact:** For anyone who can reach port 8088 — by default the whole
  LAN — this is a prepared recon sheet: kernel versions map to known CVEs,
  IPs and hostnames map the network, mountpoints describe the estate. A
  dashboard for humans is inherently a leak for attackers; the question is
  only who can reach it.
- **Decision:** Documented, no code change proposed. The tool is
  LAN-scoped by intent, and putting the read tier behind the token would
  change the product (open glanceable dashboard). Mitigations available
  without code:
  - Publish the port more tightly where that fits the household:
    `ports: - "127.0.0.1:8088:8088"` (host-local only), or a
    specific interface IP, or firewall rules on the NAS.
  - Keep the token configured anyway — it future-proofs the private tier
    that action routes will use, and rotating it kills all sessions.
  - If the exposure ever starts to matter, the read tier can move behind
    `requireToken` with the capabilities endpoint staying public.
- **False-positive notes:** None; the exposure is unconditional today.

### F4 — Failed-auth throttle sleeps inside the request goroutine

- **Rule:** GO-CONC-001 (resource-holding variant)
- **Severity:** Low
- **Location:** `internal/web/auth.go:79-88` (`throttleFailed`), cap at
  `internal/clocks/clocks.go:61` (`AuthBackoffMax = 30s`)
- **Evidence:**

  ```go
  a.mu.Unlock()

  time.Sleep(d)
  return d
  ```

- **Impact:** Each failed auth attempt parks a goroutine and a connection
  for up to 30s before the 401 is written. A LAN client can stack up
  sleeping connections cheaply. The delay itself is the anti-brute-force
  mechanism, so removing it isn't the fix — capping concurrency is.
- **Fix (optional):** Bound in-flight auth attempts with a small semaphore
  (buffered channel) acquired in `requireToken` around the throttle path, so
  at most N sleeps are ever parked. Alternatively accept it: the map is
  pruned, goroutines are cheap, and the exposure is LAN-only.
- **Regression risk:** Low if done; the success path must not acquire the
  semaphore or a full throttle pool would block legitimate logins.
- **False-positive notes:** `ReadHeaderTimeout` does not cover this — the
  sleep happens after headers are fully read.

### F5 — Unescaped IP interpolation in the frontend

- **Rule:** GO-XSS-001 (defence-in-depth)
- **Severity:** Low
- **Location:** `internal/web/index.html:826` (`addresses()` function)
- **Evidence:**

  ```js
  for (const ip of (i.ipv4 || [])) out.push(`${ip} <span class="sub">${esc(i.name)}</span>`);
  ```

  The neighbouring interface name is escaped; the IP is not.

- **Impact:** Currently none — server-side `skipAddr` drops anything
  `net.ParseIP` rejects (`internal/agent/agent.go:219-225`), and a parsed-IP
  string can only contain hex digits, colons and dots, never HTML. But the
  value originates inside a guest VM (guest agent
  `guest-network-get-interfaces`), which is a trust boundary — a compromised
  guest is exactly the adversary this design should not depend on the server
  never to change.
- **Fix:** One character: `` `${esc(ip)} <span…` ``. Makes the safety local
  to the template instead of an invariant across two packages.
- **Regression risk:** None. Escaping valid IP strings is a no-op.

### F6 — No security response headers

- **Rule:** GO-HTTP-004
- **Severity:** Low
- **Location:** `internal/web/web.go` — no header middleware exists; nothing
  sets `X-Content-Type-Options` or `X-Frame-Options`.
- **Impact:** Marginal locally — no XSS found to amplify (F5 is defence-in-
  depth), and clickjacking a LAN dashboard is exotic. These are two lines of
  free hardening, though.
- **Fix:** Set in the existing request wrapper (or a tiny middleware) so all
  routes get them:

  ```go
  w.Header().Set("X-Content-Type-Options", "nosniff")
  w.Header().Set("X-Frame-Options", "DENY")
  ```

  A CSP is optional here: the page's inline scripts
  (`internal/web/index.html:596-651`, `742-1143`) would require hash-based
  `script-src`, which is maintenance-prone for a hand-edited single file —
  skip unless there's appetite.
- **Regression risk:** None. The page is never legitimately framed.

### F7 — No `govulncheck` in CI

- **Rule:** GO-DEPLOY-001 / supply-chain hygiene
- **Severity:** Low (effort: one CI step)
- **Location:** `.github/workflows/build.yml:25-28` — vet, test, gofmt, but
  no vulnerability scan.
- **Impact:** The module graph is tiny (go-libvirt, x/crypto indirect), so
  the realistic yield is low — but the whole point of the step is catching
  the stdlib or x/crypto advisory you didn't hear about.
- **Fix:**

  ```yaml
  - name: Vulnerability scan
    run: |
      go install golang.org/x/vuln/cmd/govulncheck@latest
      govulncheck ./...
  ```

  (or `golang/govulncheck-action@v1`). Pin `@latest` consciously or pin a
  version — note `govulncheck` itself is not vendored, so a fresh runner
  fetches it each run.
- **Regression risk:** A new advisory can turn CI red with no code change —
  that's the feature.

### F8 — CI tests run without the race detector

- **Rule:** GO-CONC-001
- **Severity:** Low
- **Location:** `.github/workflows/build.yml:26` — `go test ./...`
- **Impact:** The one race in the tree (F2) would have been surfaced by
  `-race` if a test exercised the concurrent Get/SetError path.
- **Fix:** `go test -race ./...`. The build is `CGO_ENABLED=0`, but the race
  detector needs cgo at *test* time — GitHub's `ubuntu-latest` runners ship
  a working toolchain, so it works as-is.
- **Regression risk:** Slightly slower tests. No functional impact.

### F9 — Constant-time compare leaks token length *(informational)*

- **Rule:** GO-AUTH-001
- **Severity:** Informational
- **Location:** `internal/web/auth.go:100-102`
- **Evidence:**

  ```go
  return subtle.ConstantTimeCompare([]byte(candidate), []byte(a.token)) == 1
  ```

  `ConstantTimeCompare` returns 0 immediately when lengths differ, so
  response timing can theoretically distinguish a correct token length.
- **Impact:** None in practice — the docs prescribe `openssl rand -hex 32`,
  so the length (64 chars) is already public knowledge. Noted for
  completeness.
- **Fix if ever wanted:** Compare SHA-256 digests of both sides instead —
  equal length by construction, constant time by construction.
- **False-positive notes:** Not worth doing today; it would add code to
  paper over a non-secret.

### F10 — Root container holding a read-write libvirt socket — *accepted design decision*

- **Rule:** Privilege minimisation
- **Severity:** Policy / accepted risk (spec: would be High)
- **Location:** `docker-compose.yaml:13-14`:

  ```yaml
  volumes:
    - /run/truenas_libvirt:/run/truenas_libvirt
  user: "0:0"
  ```

- **Impact:** The web-facing process runs as root and holds a read-write
  libvirt handle (needed for `DomainSetMemoryStatsPeriod` and the agent
  queries). If an RCE ever existed in the HTTP layer, the blast radius is
  not "read VM stats" but "control every VM on the host". The read-only
  application code and the CI grep shrink that likelihood; the socket mount
  defines the ceiling.
- **Decision:** Documented, no change proposed — root is most likely
  required to open the TrueNAS socket, and the socket *directory* must be
  mounted rather than the socket file (libvirtd recreates it on restart;
  already documented in `docker-compose-examples.yaml:45-53`). If hardening
  later: check whether the socket has a group entry on the host (`ls -l
  /run/truenas_libvirt/`) and run the container as that gid — then drop
  `user: "0:0"`.

## Suggested order of work

| Priority | Finding | Effort |
|----------|---------|--------|
| 1 | F2 — cache race (copy-on-write) | small |
| 2 | F1 — server timeouts | small |
| 3 | F8 — `-race` in CI (then proves F2) | trivial |
| 4 | F7 — `govulncheck` in CI | trivial |
| 5 | F5 — escape `ip` in frontend | trivial |
| 6 | F6 — security headers | trivial |
| 7 | F4 — auth semaphore (optional) | small |

F3 and F10 are documented decisions, not work items.
