package settings

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestParseDirectoryRoots guards the form of storage.directory-roots: what
// it accepts, in both path forms, and each mistake it refuses, roots that
// nest among them, whatever the case of their letters.
func TestParseDirectoryRoots(t *testing.T) {
	got, err := ParseDirectoryRoots("fast=/srv/archive\nslow=D:\\archive\n")
	if err != nil {
		t.Fatal(err)
	}
	want := DirectoryRoots{{"fast", "/srv/archive"}, {"slow", "D:\\archive"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v", got)
	}
	if r, ok := got.Lookup("slow"); !ok || r.Path != "D:\\archive" {
		t.Errorf("lookup: %v %v", r, ok)
	}
	if none, err := ParseDirectoryRoots(""); err != nil || len(none) != 0 {
		t.Errorf("empty: %v %v", none, err)
	}
	for _, v := range []string{
		"fast",                      // no "="
		"Fast=/srv",                 // upper case name
		"con=/srv",                  // device name
		"a=/srv\na=/data",           // name twice
		"a=srv",                     // relative
		"a=/",                       // the root of the file system
		"a=D:\\",                    // a drive's root
		"a=/srv/",                   // separator at the end
		"a=/srv//x",                 // empty segment
		"a=/srv/./x",                // dot segment
		"a=/srv/../etc",             // dot-dot segment
		"a=/srv\tx",                 // control character
		"a=/srv\n\nb=/data",         // empty line
		"a=/srv\nb=/srv/x",          // b inside a
		"a=/srv/x\nb=/srv",          // a inside b
		"a=/SRV\nb=/srv",            // the same but for case
		"a=D:\\arch\nb=d:\\arch\\x", // nested on one drive, case folded
		"a=" + strings.Repeat("x", MaxRootPathLength),
	} {
		if _, err := ParseDirectoryRoots(v); err == nil {
			t.Errorf("%q: accepted", v)
		}
	}
	var many []string
	for i := 0; i <= MaxDirectoryRoots; i++ {
		many = append(many, "r"+strings.Repeat("a", i%40)+string(rune('a'+i%26))+"=/srv/"+string(rune('a'+i%26))+strings.Repeat("b", i))
	}
	if _, err := ParseDirectoryRoots(strings.Join(many, "\n")); err == nil {
		t.Error("more than the bound of roots accepted")
	}
	if _, err := ParseDirectoryRoots("a=C:\\x\nb=D:\\x"); err != nil {
		t.Errorf("two drives are two directories: %v", err)
	}
}

// TestParseDirectoryRootsWindowsForms guards the forms Windows reads as
// another path than their text: "/" separates segments as "\\" does, and a
// segment's final dots and spaces are dropped, so each refused value below
// holds two roots that are one directory, or one inside the other, there,
// or a root that is not the path it says. A Windows path written with "/"
// is still a path.
func TestParseDirectoryRootsWindowsForms(t *testing.T) {
	for _, v := range []string{
		"a=D:\\srv\nb=D:\\srv/x",
		"a=D:\\srv\nb=D:/srv",
		"a=D:\\srv\nb=D:\\srv.\\x",
		"a=D:\\srv\nb=D:\\srv \\x",
		"a=D:\\srv/../srv",
		"a=D:\\srv/./x",
		"a=D:\\srv\\x/",
		"a=D:\\srv\\x.",
		"a=D:/",
	} {
		if _, err := ParseDirectoryRoots(v); err == nil {
			t.Errorf("%q: accepted", v)
		}
	}
	for _, v := range []string{"a=D:/srv/x", "a=D:\\srv/x\nb=D:\\srv/y", "a=/srv/x.", "a=/srv/x "} {
		if _, err := ParseDirectoryRoots(v); err != nil {
			t.Errorf("%q: %v", v, err)
		}
	}
}

// TestParseS3Endpoints guards the form of storage.s3-endpoints: an entry and
// its flags, and each entry that cannot mean what it says.
func TestParseS3Endpoints(t *testing.T) {
	got, err := ParseS3Endpoints("https://s3.eu-central-1.amazonaws.com\nhttp://minio.lab.example:9000 plain-http private-address\n")
	if err != nil {
		t.Fatal(err)
	}
	want := S3Endpoints{{URL: "https://s3.eu-central-1.amazonaws.com"}, {URL: "http://minio.lab.example:9000", PlainHTTP: true, PrivateAddress: true}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v", got)
	}
	for _, v := range []string{
		"http://minio:9000",                   // http without plain-http
		"https://s3.example plain-http",       // plain-http on https
		"ftp://s3.example",                    // not http
		"https://s3.example  private-address", // two spaces
		"https://s3.example private-address private-address",
		"https://s3.example extra",
		" https://s3.example",
		"https://s3.example\n\nhttps://b.example",
		"https://" + strings.Repeat("a", MaxEndpointLength),
	} {
		if _, err := ParseS3Endpoints(v); err == nil {
			t.Errorf("%q: accepted", v)
		}
	}
}

// TestParseSize guards the one form of a size, each unit, its bound, and
// what it refuses.
func TestParseSize(t *testing.T) {
	for v, want := range map[string]int64{
		"0": 0, "1": 1, "1024": 1024, "1KiB": 1 << 10, "200GiB": 200 << 30, "3TiB": 3 << 40,
		"4096PiB": 1 << 62, "4611686018427387904": 1 << 62,
	} {
		if got, err := ParseSize(v); err != nil || got != want {
			t.Errorf("%q: %d, %v; want %d", v, got, err, want)
		}
	}
	for _, v := range []string{"", "-1", "1.5GiB", "01", "1 GiB", "1GB", "1gib", "GiB", "4097PiB", "4611686018427387905", "99999999999999999999"} {
		if _, err := ParseSize(v); err == nil {
			t.Errorf("%q: accepted", v)
		}
	}
	for _, n := range []int64{0, 1, 1023, 1 << 10, 3 << 30, 1<<62 - 1, 1 << 62} {
		if back, err := ParseSize(FormatSize(n)); err != nil || back != n {
			t.Errorf("%d: written %q, read back %d, %v", n, FormatSize(n), back, err)
		}
	}
}

// TestParseHeaderLimit guards receiver.header-limit: a size, at each bound
// and one byte past it.
func TestParseHeaderLimit(t *testing.T) {
	for v, want := range map[string]int64{"1MiB": 1 << 20, "64MiB": 64 << 20, "4GiB": 4 << 30, "1048576": 1 << 20} {
		if got, err := ParseHeaderLimit(v); err != nil || got != want {
			t.Errorf("%q: %d, %v; want %d", v, got, err, want)
		}
	}
	for _, v := range []string{"", "0", "1048575", "4294967297", "5GiB", "64MB", "-1MiB"} {
		if _, err := ParseHeaderLimit(v); err == nil {
			t.Errorf("%q: accepted", v)
		}
	}
}

// TestParsePixelLimit guards deid.pixel-limit: a size from 1 MiB to
// 64 GiB, nothing else.
func TestParsePixelLimit(t *testing.T) {
	for v, want := range map[string]int64{"1MiB": 1 << 20, "1GiB": 1 << 30, "64GiB": 64 << 30, "1048576": 1 << 20} {
		if got, err := ParsePixelLimit(v); err != nil || got != want {
			t.Errorf("%q: %d, %v; want %d", v, got, err, want)
		}
	}
	for _, v := range []string{"", "0", "1048575", "65GiB", "1GB", "-1MiB"} {
		if _, err := ParsePixelLimit(v); err == nil {
			t.Errorf("%q: accepted", v)
		}
	}
}

// TestParseDeidMemory guards deid.memory: a size from 256 MiB to 64 GiB,
// nothing else.
func TestParseDeidMemory(t *testing.T) {
	for v, want := range map[string]int64{"256MiB": 256 << 20, "2GiB": 2 << 30, "64GiB": 64 << 30, "268435456": 256 << 20} {
		if got, err := ParseDeidMemory(v); err != nil || got != want {
			t.Errorf("%q: %d, %v; want %d", v, got, err, want)
		}
	}
	for _, v := range []string{"", "0", "268435455", "255MiB", "65GiB", "2GB", "-1GiB"} {
		if _, err := ParseDeidMemory(v); err == nil {
			t.Errorf("%q: accepted", v)
		}
	}
}

// TestParseListCacheBytes guards deid.list-cache-bytes: a size from 64 MiB
// to 16 GiB, nothing else.
func TestParseListCacheBytes(t *testing.T) {
	for v, want := range map[string]int64{"64MiB": 64 << 20, "256MiB": 256 << 20, "16GiB": 16 << 30} {
		if got, err := ParseListCacheBytes(v); err != nil || got != want {
			t.Errorf("%q: %d, %v; want %d", v, got, err, want)
		}
	}
	for _, v := range []string{"", "0", "63MiB", "17GiB", "256MB", "-1GiB"} {
		if _, err := ParseListCacheBytes(v); err == nil {
			t.Errorf("%q: accepted", v)
		}
	}
}

// TestParseReserve guards storage.reserve: one or two terms, one of each
// kind, and that the larger applies.
func TestParseReserve(t *testing.T) {
	r, err := ParseReserve("10%,10GiB")
	if err != nil || !r.HasPercent || r.Percent != 10 || !r.HasBytes || r.Bytes != 10<<30 {
		t.Fatalf("%+v %v", r, err)
	}
	if got := r.Of(1000 << 30); got != 100<<30 {
		t.Errorf("10%% of 1000GiB: %d", got)
	}
	if got := r.Of(50 << 30); got != 10<<30 {
		t.Errorf("the size is larger on a small disk: %d", got)
	}
	if got := r.Of(0); got != 10<<30 {
		t.Errorf("an unknown total keeps the size: %d", got)
	}
	if r2, err := ParseReserve("10GiB,10%"); err != nil || r2 != r {
		t.Errorf("either order: %+v %v", r2, err)
	}
	for _, v := range []string{"5%", "0", "100%", "1TiB"} {
		if _, err := ParseReserve(v); err != nil {
			t.Errorf("%q: %v", v, err)
		}
	}
	for _, v := range []string{"", "10%,20%", "1GiB,2GiB", "101%", "10%,10GiB,1%", "10 %", "%", ",", "10%,"} {
		if _, err := ParseReserve(v); err == nil {
			t.Errorf("%q: accepted", v)
		}
	}
}

// TestParsePorts guards receiver.ports: ports and ranges, sorted, with no
// port twice, and the overlap test the allotment between tenants rests on.
func TestParsePorts(t *testing.T) {
	p, err := ParsePorts("11120-11129,11112")
	if err != nil {
		t.Fatal(err)
	}
	if p.String() != "11112,11120-11129" || !p.Contains(11112) || !p.Contains(11125) || p.Contains(11113) || p.Contains(11130) {
		t.Errorf("%v", p)
	}
	other, _ := ParsePorts("11129-11140")
	if port, ok := p.Overlap(other); !ok || port != 11129 {
		t.Errorf("overlap %d %v", port, ok)
	}
	apart, _ := ParsePorts("11130,104")
	if _, ok := p.Overlap(apart); ok {
		t.Error("ports apart overlap")
	}
	if none, err := ParsePorts(""); err != nil || len(none) != 0 || none.Contains(104) {
		t.Errorf("empty: %v %v", none, err)
	}
	for _, v := range []string{"0", "65536", "11112,11112", "11110-11120,11115", "11120-11110", "011112", "11112 ", "a", ",", "1-", "-1", "1--2"} {
		if _, err := ParsePorts(v); err == nil {
			t.Errorf("%q: accepted", v)
		}
	}
}

// TestParseBindAddresses guards receiver.bind-addresses: one spelling per
// address, no zone, no IPv4-mapped form, none twice.
func TestParseBindAddresses(t *testing.T) {
	b, err := ParseBindAddresses("192.0.2.10,2001:db8::1")
	if err != nil || len(b) != 2 || b.String() != "192.0.2.10,2001:db8::1" {
		t.Fatalf("%v %v", b, err)
	}
	for _, v := range []string{"192.0.2.10,192.0.2.10", "2001:DB8::1", "2001:db8:0::1", "fe80::1%eth0", "::ffff:192.0.2.1", "192.0.2.010", "192.0.2.10, 2001:db8::1", "host"} {
		if _, err := ParseBindAddresses(v); err == nil {
			t.Errorf("%q: accepted", v)
		}
	}
}

// TestParseRetention guards events.retention's bounds.
func TestParseRetention(t *testing.T) {
	for v, ok := range map[string]bool{"1d": true, "24h": true, "23h": false, "3650d": true, "3651d": false, "90d": true, "": false} {
		if _, err := ParseRetention(v); (err == nil) != ok {
			t.Errorf("%q: accepted %v, want %v", v, err == nil, ok)
		}
	}
	if d, _ := ParseRetention("90d"); d != 90*24*time.Hour {
		t.Errorf("90d is %v", d)
	}
}

// FuzzParsePorts guards the ports parser against any input: it never
// panics, and what it accepts it writes back in a form it reads as the same
// ports.
func FuzzParsePorts(f *testing.F) {
	for _, s := range []string{"", "11112", "11112,11120-11129", "1-65535", "5,4,3", "1-2,2-3"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		p, err := ParsePorts(s)
		if err != nil {
			return
		}
		back, err := ParsePorts(p.String())
		if err != nil || !reflect.DeepEqual(back, p) {
			t.Fatalf("%q read as %v, written %q, read back %v (%v)", s, p, p.String(), back, err)
		}
		for _, r := range p {
			if r.Lo < 1 || r.Hi > 65535 || r.Lo > r.Hi {
				t.Fatalf("%q gave the range %v", s, r)
			}
		}
	})
}

// FuzzParseSize guards the size parser: it never panics, a size it accepts
// is 0 to 2^62, and FormatSize writes it back as the same size.
func FuzzParseSize(f *testing.F) {
	for _, s := range []string{"0", "200GiB", "4096PiB", "1KiB", "18446744073709551615", "9PiB"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		n, err := ParseSize(s)
		if err != nil {
			return
		}
		if n < 0 || n > MaxSize {
			t.Fatalf("%q is %d", s, n)
		}
		if back, err := ParseSize(FormatSize(n)); err != nil || back != n {
			t.Fatalf("%q is %d, written %q, read back %d (%v)", s, n, FormatSize(n), back, err)
		}
	})
}

// FuzzParseHeaderLimit guards the header bound's parser: it never panics,
// a bound it accepts is within its limits, and FormatSize writes it back as
// the same bound.
func FuzzParseHeaderLimit(f *testing.F) {
	for _, s := range []string{"64MiB", "1MiB", "4GiB", "4294967297", "0", "1KiB"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		n, err := ParseHeaderLimit(s)
		if err != nil {
			return
		}
		if n < MinHeaderLimit || n > MaxHeaderLimit {
			t.Fatalf("%q is %d", s, n)
		}
		if back, err := ParseHeaderLimit(FormatSize(n)); err != nil || back != n {
			t.Fatalf("%q is %d, written %q, read back %d (%v)", s, n, FormatSize(n), back, err)
		}
	})
}

// FuzzParseReserve guards the reserve parser: it never panics, and what it
// accepts it writes back as the same reserve.
func FuzzParseReserve(f *testing.F) {
	for _, s := range []string{"10%,10GiB", "5%", "1TiB", "100%,0"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		r, err := ParseReserve(s)
		if err != nil {
			return
		}
		if back, err := ParseReserve(r.String()); err != nil || back != r {
			t.Fatalf("%q read as %+v, written %q, read back %+v (%v)", s, r, r.String(), back, err)
		}
	})
}

// FuzzParseDirectoryRoots guards the roots parser: it never panics, what it
// accepts it writes back as the same roots, and no two roots it accepts
// nest.
func FuzzParseDirectoryRoots(f *testing.F) {
	for _, s := range []string{"fast=/srv/archive", "a=/x\nb=/y\n", "a=D:\\x", "a=/x\nb=/x/y"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		r, err := ParseDirectoryRoots(s)
		if err != nil {
			return
		}
		back, err := ParseDirectoryRoots(r.String())
		if err != nil || !reflect.DeepEqual(back, r) && (len(back) != 0 || len(r) != 0) {
			t.Fatalf("%q read as %v, read back %v (%v)", s, r, back, err)
		}
		for i := range r {
			for j := i + 1; j < len(r); j++ {
				if pathsNest(r[i].Path, r[j].Path) {
					t.Fatalf("%q: %v and %v nest", s, r[i], r[j])
				}
			}
		}
	})
}

// FuzzParseS3Endpoints guards the endpoints parser: it never panics, and
// what it accepts it writes back as the same entries.
func FuzzParseS3Endpoints(f *testing.F) {
	for _, s := range []string{"https://s3.example", "http://m:9000 plain-http private-address\nhttps://b"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		e, err := ParseS3Endpoints(s)
		if err != nil {
			return
		}
		back, err := ParseS3Endpoints(e.String())
		if err != nil || !reflect.DeepEqual(back, e) && (len(back) != 0 || len(e) != 0) {
			t.Fatalf("%q read as %+v, read back %+v (%v)", s, e, back, err)
		}
	})
}

// FuzzParseBindAddresses guards the addresses parser: it never panics, and
// what it accepts it writes back as the same text.
func FuzzParseBindAddresses(f *testing.F) {
	for _, s := range []string{"192.0.2.1", "192.0.2.1,2001:db8::1", "::"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		b, err := ParseBindAddresses(s)
		if err != nil {
			return
		}
		if b.String() != s {
			t.Fatalf("%q written back as %q", s, b.String())
		}
	})
}
