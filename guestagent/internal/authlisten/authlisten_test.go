package authlisten

import (
	"bufio"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

const token = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// echoServer accepts authenticated connections and echoes what it reads, so a
// test can prove that the bytes following the preamble reach the protocol
// intact and nothing else does.
func echoServer(t *testing.T) (*Listener, string) {
	t.Helper()
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l := Wrap(inner, token)
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
	return l, inner.Addr().String()
}

func TestAcceptsCorrectPreambleAndPreservesFollowingBytes(t *testing.T) {
	_, addr := echoServer(t)
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// Preamble and payload in ONE write: the same TCP segment. This is the case
	// a buffered reader would get wrong by swallowing "hello".
	if _, err := conn.Write([]byte(Preamble(token) + "hello\n")); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	got, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if got != "hello\n" {
		t.Fatalf("echo = %q, want %q", got, "hello\n")
	}
}

func TestRejectsWrongToken(t *testing.T) {
	_, addr := echoServer(t)
	for _, line := range []string{
		Preamble(strings.Repeat("f", len(token))), // right shape, wrong secret
		Preamble(token[:len(token)-1]),            // one byte short
		Preamble(token + "0"),                     // one byte long
		"GET /healthz HTTP/1.1\r\n",               // no preamble at all
		Tag + "\n",                                // tag without a token
	} {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = conn.Write([]byte(line + "hello\n"))
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		buf := make([]byte, 16)
		n, err := conn.Read(buf)
		conn.Close()
		if err == nil || n > 0 {
			t.Fatalf("line %q: connection was served (read %d bytes, err %v); want it closed", line, n, err)
		}
	}
}

func TestRejectsOverlongLine(t *testing.T) {
	_, addr := echoServer(t)
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = conn.Write([]byte(strings.Repeat("A", maxLine+10)))
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if n, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatalf("overlong preamble was served (read %d bytes)", n)
	}
}

func TestSilentClientDoesNotBlockOthers(t *testing.T) {
	_, addr := echoServer(t)
	// A client that connects and says nothing.
	silent, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer silent.Close()

	// A well-behaved client right behind it must still be served promptly.
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = conn.Write([]byte(Preamble(token) + "x\n"))
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	got, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil || got != "x\n" {
		t.Fatalf("second client not served while first is silent: %q %v", got, err)
	}
}

func TestCloseUnblocksAccept(t *testing.T) {
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l := Wrap(inner, token)
	done := make(chan error, 1)
	go func() {
		_, err := l.Accept()
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	l.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Accept returned a conn after Close")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Accept did not return after Close")
	}
}
