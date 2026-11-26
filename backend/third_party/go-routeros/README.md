# go-routeros v3.0.1 — vendored copy with local changes

This directory is a copy of `github.com/go-routeros/routeros/v3` at **v3.0.1**
(MIT, see `LICENSE`), used by the backend through a `replace` in
`backend/go.mod`. It is not the upstream module: the changes below are local.
Each one is marked "local patch" in the code.

## Local changes

- **Caps checked before allocation** (`proto/limits.go`, `proto/reader.go`,
  `reply.go`): one word ≤ 1 MiB (`proto.MaxWordLen`), the wire bytes of one
  reply ≤ 32 MiB (`proto.MaxReplyLen`), one reply ≤ 50,000 `!re` rows
  (`MaxReplyRows`). Upstream allocated whatever length a word declared (up to
  4 GiB) and collected rows without bound. A violation is a
  `*proto.LimitError`.
- **Protocol errors are typed** (`*proto.ProtocolError`): an invalid sentence
  word (quoted up to 64 bytes) and a reserved length byte (`0xF8`–`0xFF`,
  which upstream read as a length).
- **Session-fatal errors** (`error.go`, `run.go`, `client.go`): a `!fatal`, a
  cap violation, a protocol error, an unknown reply word and any transport
  error in the middle of a command come back wrapped in
  `*SessionFatalError`. The client closes its connection before returning
  one and answers every later command with it — a desynchronised stream is
  never read again. The wrapper's text is the wrapped error's, and it
  unwraps, so `errors.Is` / `errors.As` on the cause keep working.
- **`proto.Reader.BeginReply`**: starts a reply's byte budget (called per
  command in sync mode, per sentence in async mode).
- **Async mode registers a reply's tag before the device can answer**
  (`run.go`): upstream sent the sentence, then registered the tag, and
  dropped rows that arrived in between — its own `TestProtoRunAsync` failed
  about once in a hundred runs. (The backend uses sync mode only; the fix is
  here so that this copy's test suite is not flaky.)
- **Buffered reads take no goroutine** (`proto/io_context.go`): the reader
  asks for every length byte and every word separately, and upstream started
  a goroutine, a channel and a copy buffer for each of those reads so that a
  blocked one could be cancelled — some 400,000 for a reply of 50,000 rows.
  A read the `bufio.Reader` can satisfy from its buffer cannot block and is
  now answered directly; only a read that has to wait for the peer goes
  through the goroutine, and after one of those was abandoned (cancel, close)
  the direct path is not used again.
- **No dialers**: `Dial`, `DialTimeout`, `DialContext`, `DialTLS`,
  `DialTLSTimeout` and `DialTLSContext` are removed. The backend opens every
  connection itself (dial policy, timeouts, pinned certificate) and passes it
  to `NewClient`.
- **Removed files**: `client_test.go` (tests against a live device and
  non-loopback dials), `examples/`, `.github/`.
- **Removed tests**: `TestRandomData` and `TestReadRandom`. Both fed the
  reader `crypto/rand` bytes and asserted one outcome, so they failed a few
  runs in a hundred (upstream too); the fuzz targets cover arbitrary input.
- **Added tests**: `limits_test.go`, `proto/limits_test.go` (incl.
  `FuzzReadSentence`). The backend's own `FuzzSentence`
  (`backend/internal/routeros`, `make fuzz-ros`) fuzzes the reader and the
  sync reply loop together.

Run this copy's tests with `cd backend/third_party/go-routeros && go test ./...`
(`make test` does).

---

# RouterOS Client for the Go language

Go library for accessing Mikrotik devices using the RouterOS API.

API documentation is available at [pkg.go.dev](https://pkg.go.dev/github.com/go-routeros/routeros/v3).  
Page on the [Mikrotik Wiki](http://wiki.mikrotik.com/wiki/API_in_Go).

Usage of `gopkg.in` was removed in favor of Go modules. Please, update you import paths to
`github.com/go-routeros/routeros/v3`.

Current version:
[**v3 and newest**](https://github.com/go-routeros/routeros/),

Old released versions:
[**v2**](https://github.com/go-routeros/routeros/tree/v2),
[**v1**](https://github.com/go-routeros/routeros/tree/v1)

To install it, run:
`go get github.com/go-routeros/routeros/v3`
