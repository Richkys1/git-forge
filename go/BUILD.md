# Building the Go core

The forge's server is a vendored static Go binary (`../forge`, committed in the
repository root). The grader never needs Go: `main.ts` spawns the binary.

## Build command (byte-reproducible)

From this directory (`go/`), with Go 1.23+ on PATH:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags '-s -w' -o ../forge .
```

- `CGO_ENABLED=0` — pure Go, statically linked (no libc).
- `-trimpath` — no build-machine paths in the binary, so the same source builds
  the same bytes anywhere.
- `-ldflags '-s -w'` — strip the symbol table and DWARF (smaller binary).
- The module has **no dependencies** (stdlib only), so no network is needed.

Verify the result:

```sh
file ../forge        # ELF 64-bit LSB executable, x86-64, statically linked
../forge             # prints "[forge] listening on port N" and serves
```

Rebuilding from the same source with the same Go version reproduces the binary
byte for byte (Go builds are reproducible given the same toolchain; `-trimpath`
removes the one machine-dependent input).

## Layout

- `main.go` — entrypoint: bind :0, print the port, accept loop.
- `http.go` — hand-rolled HTTP/1.1 over raw TCP + routing (port of app.ts).
- `store.go` — in-memory store, deterministic clock (port of lib/store.ts).
- `git.go` — objects, trees, commits, packs (port of lib/git.ts).
- `pkt.go` — pkt-line framing (port of lib/pkt.ts).
- `gitsmart.go` — smart HTTP v2/v0 + receive-pack (port of lib/gitsmart.ts).
- `rest.go` — REST v3 (port of lib/rest.ts).
- `graphql.go` — GraphQL v4 parser + resolver (port of lib/graphql.ts).
- `mcp.go` — MCP JSON-RPC (port of lib/mcp.ts).

The TypeScript files (`app.ts`, `lib/`) remain in the repository as the
reference implementation the port was written from; the live server is the
Go binary, spawned by `main.ts`.

## Verification (2026-11-17, this VM)

Reproducibility: two builds from this source give identical sha256
`4accd8bb0743b1305ea2548eb3b5f6590eb814860c3a1aecfdbcfa72d1cce36d`.

grade-local (`--driver process`, practice trials), all runs 120/120,
feature 600/600, agent 200/200, replay byte-identical, L1 regrade reproduced:

| run | p50 (us) | perf | score |
| --- | --- | --- | --- |
| Go, single trial (before empty-list fix) | 213,632 | 192 | 987 |
| Go, single trial (after fix) | 197,574 | 196 | 996 |
| Go, 2-core pinned | 179,468 | 200 | 1000 |
| Go, 3 trials mean | 193,790 | 192 | 992 |
| Node HEAD (b509ba1), 3 trials mean | 208,000 | 192 | 992 |

3-trial per-workload means (ms): Go push 284.5 / clone 90.4 / read 567.4 /
write 98.5; Node push 355.3 / clone 93.9 / read 575.9 / write 98.4.
Go's edge is almost entirely push (-20%); clone/read/write are tied.
Raw-server benchmark (Go client, keep-alive): GET issues 54.4 us/req vs
23.4 us/req for a trivial canned-response server — the suite's Node client
adds ~280 us/req on top, so the local p50 is client-dominated, not
server-dominated.

Board projection: the task's target (local p50 well under ~150 us) was NOT
met — the local p50 is ~194 us because the suite's client costs dominate.
Uniform-slowdown model (board ~2.46x local, as measured for Node):
board p50 ~477 us, perf ~137, score ~937 — no real gain over the current
932. Contention model (the leaderboard's top Go entries run 322 us on the
board, i.e. a ~1.6-1.7x board factor, because a Go server leaves the shared
pod's CPU to the suite's clients): board p50 ~330 us, perf ~153, score
~953 — just above the ~950 top-60 cut. The two models cannot be
distinguished from this VM; the board outcome is genuinely uncertain.
The port is NOT a regression (local score equals Node's 991.7, feature
600/600, deterministic), so the downside of submitting is small, but the
~950 target is not credibly reached on local evidence alone.
