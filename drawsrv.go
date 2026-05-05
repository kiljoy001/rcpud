//go:build drawsrv

package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"unsafe"
)

type FB struct {
	fd   int
	data []byte
	info struct{ W, H, Bpp, Stride int }
	mu   sync.RWMutex
}

func openFB() (*FB, error) {
	fd, err := syscall.Open("/dev/fb0", syscall.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	var vi struct {
		X, Y, Xv, Yv, Xo, Yo, Bpp uint32
		_ [44]byte
	}
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), 0x4600, uintptr(unsafe.Pointer(&vi))); e != 0 {
		syscall.Close(fd)
		return nil, e
	}
	f := &FB{fd: fd}
	f.info.W, f.info.H, f.info.Bpp = int(vi.X), int(vi.Y), int(vi.Bpp)
	f.info.Stride = f.info.W * (f.info.Bpp / 8)
	log.Printf("fb: %dx%d bpp=%d stride=%d", f.info.W, f.info.H, f.info.Bpp, f.info.Stride)
	f.data, err = syscall.Mmap(fd, 0, f.info.Stride*f.info.H,
		syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		syscall.Close(fd)
		return nil, err
	}
	return f, nil
}

func (f *FB) Copy() []byte {
	f.mu.RLock()
	defer f.mu.RUnlock()
	p := make([]byte, len(f.data))
	copy(p, f.data)
	return p
}

func (f *FB) Close() {
	if f.data != nil {
		syscall.Munmap(f.data)
	}
	if f.fd >= 0 {
		syscall.Close(f.fd)
	}
}

// wsysmsg types
const (
	Rerror = 1
	Trdmouse = 2
	Rrdmouse = 3
	Tmoveto = 4
	Rmoveto = 5
	Trdkbd = 10
	Rrdkbd = 11
	Tinit = 14
	Rinit = 15
	Trddraw = 20
	Rrddraw = 21
	Ttop = 24
)

type M struct {
	T, Tag uint8
	X, Y, B int32
	Ms uint32
	R int32
	N uint32
	D []byte
	Ws, Lbl string
}

func p32(b []byte, v uint32) { binary.BigEndian.PutUint32(b, v) }
func g32(b []byte) uint32    { return binary.BigEndian.Uint32(b) }
func p16(b []byte, v uint16) { binary.BigEndian.PutUint16(b, v) }
func g16(b []byte) uint16    { return binary.BigEndian.Uint16(b) }

func pstr(b []byte, s string) int {
	p16(b, uint16(len(s)))
	copy(b[2:], s)
	return 2 + len(s)
}

func gstr(b []byte, o int) (int, string) {
	if o+2 > len(b) { return o, "" }
	n := int(g16(b[o:]))
	o += 2
	if o+n > len(b) { n = len(b) - o; if n < 0 { n = 0 } }
	return o + n, string(b[o:o+n])
}

func sz(m *M) int {
	n := 2
	switch m.T {
	case Rrdmouse: n += 17
	case Rrdkbd: n += 2
	case Rrddraw: n += 4 + int(m.N)
	case Rerror: n += 2 + len(m.Lbl)
	}
	return n
}

func wenc(m *M, b []byte) int {
	b[0], b[1] = m.T, m.Tag
	n := 2
	switch m.T {
	case Rrdmouse:
		p32(b[n:], uint32(m.X)); n += 4
		p32(b[n:], uint32(m.Y)); n += 4
		p32(b[n:], uint32(m.B)); n += 4
		p32(b[n:], m.Ms); n += 4
		b[n] = 0; n++
	case Rrdkbd:
		p16(b[n:], uint16(m.R)); n += 2
	case Rrddraw:
		p32(b[n:], m.N); n += 4
		copy(b[n:], m.D[:m.N]); n += int(m.N)
	case Rerror:
		n += pstr(b[n:], m.Lbl)
	}
	return n
}

func wdec(b []byte, m *M) int {
	if len(b) < 2 { return -1 }
	m.T, m.Tag = b[0], b[1]
	n := 2
	switch m.T {
	case Tinit:
		n, m.Ws = gstr(b, n)
		n, m.Lbl = gstr(b, n)
		_, _ = gstr(b, n)
	case Trdmouse, Trdkbd, Ttop:
	case Tmoveto:
		if n+8 <= len(b) {
			m.X = int32(g32(b[n:])); n += 4
			m.Y = int32(g32(b[n:])); n += 4
		}
	case Trddraw:
		if n+4 <= len(b) {
			m.N = g32(b[n:]); n += 4
		}
	}
	return n
}

type UI struct{ fd int }

func openUI() (*UI, error) {
	fd, err := syscall.Open("/dev/uinput", syscall.O_RDWR, 0)
	if err != nil { return nil, err }
	ub := func(i uintptr, bits ...int) {
		var b [16]uint32
		for _, v := range bits { b[v/32] |= 1 << (v % 32) }
		syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), i, uintptr(unsafe.Pointer(&b[0])))
	}
	ub(0x40045564, 1, 3, 0)
	ub(0x40045565, 0x110, 0x111, 0x112)
	ub(0x40045567, 0, 1)
	type ai struct{ V, Mn, Mx, Fz, Fl, R int32 }
	x := ai{Mx: 8000}; y := ai{Mx: 4000}
	syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), 0x40085560, uintptr(unsafe.Pointer(&x)))
	syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), 0x40085561, uintptr(unsafe.Pointer(&y)))
	type ud struct {
		N  [80]byte
		ID struct{ B, Vr, Pd, Vn uint16 }
	}
	var d ud
	copy(d.N[:], "drawsrv-mouse")
	d.ID.B, d.ID.Vr, d.ID.Pd, d.ID.Vn = 3, 0x9f9f, 1, 1
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), 0x5503, uintptr(unsafe.Pointer(&d))); e != 0 {
		syscall.Close(fd); return nil, e
	}
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), 0x5501, 0); e != 0 {
		syscall.Close(fd); return nil, e
	}
	return &UI{fd: fd}, nil
}

type ie struct {
	_ [16]byte
	T, C uint16
	V int32
}

func (u *UI) ev(t, c uint16, v int32) {
	e := ie{T: t, C: c, V: v}
	syscall.Syscall(syscall.SYS_WRITE, uintptr(u.fd), uintptr(unsafe.Pointer(&e)), unsafe.Sizeof(e))
}
func (u *UI) Move(x, y int32) { u.ev(3, 0, x); u.ev(3, 1, y); u.ev(0, 0, 0) }
func (u *UI) Close() {
	if u.fd >= 0 { syscall.Syscall(syscall.SYS_IOCTL, uintptr(u.fd), 0x5502, 0); syscall.Close(u.fd) }
}

func handle(conn net.Conn, fb *FB, ui *UI) {
	defer conn.Close()
	h := make([]byte, 4)
	b := make([]byte, 256*1024*1024) // 256MB for big framebuffers
	for {
		if _, err := conn.Read(h); err != nil { return }
		n := int(g32(h))
		if n < 2 || n > len(b) { return }
		for t := 0; t < n; {
			r, err := conn.Read(b[t:n])
			if err != nil { return }
			t += r
		}
		var m M
		wdec(b[:n], &m)
		var r M
		switch m.T {
		case Tinit:
			log.Printf("init: %s %s", m.Ws, m.Lbl)
			r.T, r.Tag = Rinit, m.Tag
		case Trdmouse:
			r.T, r.Tag = Rrdmouse, m.Tag
		case Trdkbd:
			r.T, r.Tag = Rrdkbd, m.Tag
		case Trddraw:
			r.T, r.Tag = Rrddraw, m.Tag
			if fb != nil {
				p := fb.Copy()
				r.N = uint32(len(p))
				r.D = p
			}
		case Tmoveto:
			if ui != nil { ui.Move(m.X, m.Y) }
			r.T, r.Tag = Rmoveto, m.Tag
		default:
			r.T, r.Tag = Rerror, m.Tag
			r.Lbl = fmt.Sprintf("unhandled %d", m.T)
		}
		rs := sz(&r)
		rb := make([]byte, rs+4)
		p32(rb, uint32(rs))
		wenc(&r, rb[4:])
		conn.Write(rb)
	}
}

func main() {
	tcpAddr := flag.String("tcp", "", "TCP listen address (e.g. :17029)")
	unixSock := flag.String("unix", "", "Unix socket path (default: $NAMESPACE/drawsrv)")
	flag.Parse()

	fb, _ := openFB()
	ui, _ := openUI()
	if ui != nil { defer ui.Close() }

	if *tcpAddr != "" {
		ln, err := net.Listen("tcp", *tcpAddr)
		if err != nil { log.Fatal(err) }
		log.Printf("drawsrv TCP on %s", *tcpAddr)
		for {
			c, err := ln.Accept()
			if err != nil { log.Fatal(err) }
			go handle(c, fb, ui)
		}
	} else {
		sp := *unixSock
		if sp == "" {
			ns := os.Getenv("NAMESPACE")
			if ns == "" { ns = "/tmp" }
			sp = filepath.Join(ns, "drawsrv")
		}
		os.Remove(sp)
		ln, err := net.Listen("unix", sp)
		if err != nil { log.Fatal(err) }
		log.Printf("drawsrv unix on %s", sp)
		for {
			c, err := ln.Accept()
			if err != nil { log.Fatal(err) }
			go handle(c, fb, ui)
		}
	}
}
