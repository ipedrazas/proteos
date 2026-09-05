// Package authlisten gates a listener behind a shared-secret preamble.
//
// Inside a Firecracker microVM the guest's listeners need no authentication:
// vsock has no network identity and reaches exactly one process on the host.
// On the Kubernetes backend the same listeners are TCP sockets on a pod IP, and
// the only thing keeping another pod out of a machine's terminal is a
// NetworkPolicy — a control that fails OPEN on a CNI that does not enforce it,
// and that nothing in the data path can observe. This package is the second
// control: the node-agent that created the pod knows a per-machine secret and
// writes it as the first line of every connection; anything that does not is
// closed before a single byte reaches the protocol behind the listener.
//
// The frame is deliberately trivial — one line, constant-time compared — so it
// costs nothing on the terminal's latency path and cannot be confused with the
// HTTP, WebSocket or SSH traffic that follows it. It is a guest-agent wire
// contract shared with nodeagent/api (GuestAuthPreamble); the two modules do
// not import each other, so the format is fixed here in words: the ASCII tag,
// one space, the token, '\n'.
package authlisten

import (
	"crypto/subtle"
	"errors"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"
)

// Tag is the first word of the preamble line.
const Tag = "PROTEOS-GUEST-AUTH"

// Preamble renders the line the dialer writes for token.
func Preamble(token string) string { return Tag + " " + token + "\n" }

// handshakeTimeout bounds how long a fresh connection may take to present the
// preamble. Generous for a dialer on the same cluster network; short enough that
// a connection that never speaks cannot hold resources.
const handshakeTimeout = 5 * time.Second

// maxLine bounds the preamble read. A real line is Tag + space + a 64-hex token
// + newline; anything longer is not ours.
const maxLine = 256

// Listener wraps an inner net.Listener and yields only connections whose first
// line is the expected preamble. Failed handshakes are closed and logged; they
// never surface to Accept.
type Listener struct {
	inner net.Listener
	token []byte

	accepted chan net.Conn
	done     chan struct{}
	once     sync.Once
	err      error
	mu       sync.Mutex
}

// Wrap returns a Listener gating inner behind token. An empty token is a
// programming error — callers decide whether auth is configured at all and
// should not wrap when it is not.
func Wrap(inner net.Listener, token string) *Listener {
	l := &Listener{
		inner:    inner,
		token:    []byte(token),
		accepted: make(chan net.Conn),
		done:     make(chan struct{}),
	}
	go l.loop()
	return l
}

// loop accepts from the inner listener and hands each connection to its own
// handshake goroutine, so a client that stalls on the preamble never blocks the
// accept path for the next one.
func (l *Listener) loop() {
	for {
		conn, err := l.inner.Accept()
		if err != nil {
			l.mu.Lock()
			l.err = err
			l.mu.Unlock()
			l.once.Do(func() { close(l.done) })
			return
		}
		go l.handshake(conn)
	}
}

// handshake reads exactly one line — byte by byte, so no protocol bytes that
// follow the newline are consumed — and compares it in constant time.
func (l *Listener) handshake(conn net.Conn) {
	_ = conn.SetReadDeadline(time.Now().Add(handshakeTimeout))
	line, err := readLine(conn, maxLine)
	if err != nil {
		slog.Warn("authlisten: rejected connection", "remote", conn.RemoteAddr().String(), "err", err)
		conn.Close()
		return
	}
	_ = conn.SetReadDeadline(time.Time{})

	if !l.valid(line) {
		slog.Warn("authlisten: rejected connection: bad preamble", "remote", conn.RemoteAddr().String())
		conn.Close()
		return
	}

	select {
	case l.accepted <- conn:
	case <-l.done:
		conn.Close()
	}
}

// valid checks the preamble without leaking, through timing, how much of the
// token matched. The tag and length are checked first: they are not secret.
func (l *Listener) valid(line string) bool {
	rest, ok := strings.CutPrefix(line, Tag+" ")
	if !ok {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(rest), l.token) == 1
}

// readLine reads up to and including the first '\n', one byte at a time, and
// returns the line without it. Reading a byte at a time is what makes the
// wrapper transparent: a bufio.Reader would swallow whatever the client sent
// right behind the preamble, and that belongs to the protocol, not to us.
func readLine(conn net.Conn, max int) (string, error) {
	var buf [1]byte
	line := make([]byte, 0, 96)
	for len(line) < max {
		n, err := conn.Read(buf[:])
		if err != nil {
			return "", err
		}
		if n == 0 {
			continue
		}
		if buf[0] == '\n' {
			return string(line), nil
		}
		line = append(line, buf[0])
	}
	return "", errors.New("preamble line too long")
}

// Accept returns the next authenticated connection.
func (l *Listener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.accepted:
		return conn, nil
	case <-l.done:
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.err != nil {
			return nil, l.err
		}
		return nil, net.ErrClosed
	}
}

// Close closes the inner listener; in-flight handshakes are dropped.
func (l *Listener) Close() error {
	err := l.inner.Close()
	l.once.Do(func() { close(l.done) })
	return err
}

// Addr is the inner listener's address.
func (l *Listener) Addr() net.Addr { return l.inner.Addr() }
