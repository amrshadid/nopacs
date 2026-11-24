package server

import (
	"net"
	"testing"
	"time"
)

// TestLimitListener guards the bound on network connections: with room for
// two, a third connection is not accepted until one of the two is closed,
// and a waiting Accept ends when the listener closes.
func TestLimitListener(t *testing.T) {
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l := newLimitListener(inner, 2)
	accepted := make(chan net.Conn, 3)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				close(accepted)
				return
			}
			accepted <- c
		}
	}()
	var clients []net.Conn
	for range 3 {
		c, err := net.Dial("tcp", inner.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		clients = append(clients, c)
	}
	first, second := <-accepted, <-accepted
	select {
	case <-accepted:
		t.Fatal("a third connection was accepted with room for two")
	case <-time.After(200 * time.Millisecond):
	}
	_ = first.Close()
	select {
	case third := <-accepted:
		_ = third.Close()
	case <-time.After(2 * time.Second):
		t.Fatal("closing a connection did not free its slot")
	}
	_ = second.Close()
	_ = l.Close()
	for range accepted {
	}
	for _, c := range clients {
		_ = c.Close()
	}
}

// TestMaxNetworkConns guards the bound's arithmetic: the file limit less
// the reserve, never under the minimum.
func TestMaxNetworkConns(t *testing.T) {
	n := maxNetworkConns()
	if limit := fileLimit(); limit > 0 {
		if want := max(minNetworkConns, limit-fileReserve); n != want {
			t.Fatalf("with a file limit of %d the bound is %d, want %d", limit, n, want)
		}
	} else if n != defaultNetworkConns {
		t.Fatalf("with no file limit the bound is %d, want %d", n, defaultNetworkConns)
	}
}
