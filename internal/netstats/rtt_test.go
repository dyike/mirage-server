package netstats

import (
	"crypto/tls"
	"io"
	"net"
	"runtime"
	"testing"
	"time"
)

func TestRTTOnExistingTCPConnection(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	conn, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	peer, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	conn.SetDeadline(time.Now().Add(time.Second))
	peer.SetDeadline(time.Now().Add(time.Second))
	if _, err := conn.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(peer, make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := peer.Write([]byte{2}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(conn, make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	for _, c := range []net.Conn{conn, tls.Client(conn, &tls.Config{})} {
		rtt, known := RTT(c)
		if runtime.GOOS == "linux" || runtime.GOOS == "android" || runtime.GOOS == "darwin" || runtime.GOOS == "ios" {
			if !known || rtt <= 0 || rtt > time.Second {
				t.Fatalf("invalid local TCP RTT: %v, known=%v", rtt, known)
			}
		} else if known {
			t.Fatal("unsupported platform invented a sample")
		}
	}
	conn.Close()
	if rtt, known := RTT(conn); known || rtt != 0 {
		t.Fatal("closed connection retained a sample", rtt)
	}
}

func TestUnsupportedTransportHasNoRTT(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	for _, c := range []net.Conn{nil, a} {
		if rtt, known := RTT(c); known || rtt != 0 {
			t.Fatal("unsupported connection invented a sample", rtt)
		}
	}
}
