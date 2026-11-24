package dir

import (
	"bytes"
	"context"
	"io"
	"net"
	"testing"
)

// TestSendTo guards the stream's own copy of its next bytes: what SendTo
// writes is what Read would have given, from wherever earlier reads left
// the stream, Remaining counts down with both, a SendTo past the range's
// end writes only what is left, and the reads after it go on from there.
// Over a TCP connection it is the bytes the system's copy from file to
// socket carries, compared on the other end.
func TestSendTo(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	data := make([]byte, 3<<20)
	for i := range data {
		data[i] = byte(i % 251)
	}
	if _, err := s.Put(ctx, "a/obj", bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	const offset, length = 1000, 2 << 20
	want := data[offset : offset+length]
	rc, err := s.Read(ctx, "a/obj", offset, length)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	sec := rc.(*section)
	if sec.Remaining() != length {
		t.Fatalf("Remaining at the start: %d, want %d", sec.Remaining(), length)
	}
	head := make([]byte, 100)
	if _, err := io.ReadFull(rc, head); err != nil || !bytes.Equal(head, want[:100]) {
		t.Fatalf("the first read: %v", err)
	}
	var out bytes.Buffer
	n, err := sec.SendTo(&out, 1<<20)
	if err != nil || n != 1<<20 || !bytes.Equal(out.Bytes(), want[100:100+1<<20]) {
		t.Fatalf("SendTo after a read: %d, %v, same bytes %v", n, err, bytes.Equal(out.Bytes(), want[100:100+1<<20]))
	}
	if left := sec.Remaining(); left != length-100-1<<20 {
		t.Fatalf("Remaining after SendTo: %d", left)
	}
	next := make([]byte, 10)
	if _, err := io.ReadFull(rc, next); err != nil || !bytes.Equal(next, want[100+1<<20:110+1<<20]) {
		t.Fatalf("a read after SendTo does not go on from it: %v", err)
	}

	// Over a socket: the rest of the range, asked for with more than is left.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	got := make(chan []byte, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			got <- nil
			return
		}
		b, _ := io.ReadAll(c)
		_ = c.Close()
		got <- b
	}()
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	rest := want[110+1<<20:]
	n, err = sec.SendTo(conn, length)
	_ = conn.Close()
	if err != nil || n != int64(len(rest)) {
		t.Fatalf("SendTo past the end over a socket: %d, %v, want %d", n, err, len(rest))
	}
	if b := <-got; !bytes.Equal(b, rest) {
		t.Fatalf("the other end of the socket got %d bytes, not the range's last %d", len(b), len(rest))
	}
	if sec.Remaining() != 0 {
		t.Fatalf("Remaining at the end: %d", sec.Remaining())
	}
	if n, err := sec.SendTo(&out, 1); n != 0 || err != nil {
		t.Fatalf("SendTo at the end: %d, %v", n, err)
	}
	if n, err := rc.Read(next); n != 0 || err != io.EOF {
		t.Fatalf("Read at the end: %d, %v", n, err)
	}
}
