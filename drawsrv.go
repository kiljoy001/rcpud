//go:build drawsrv
// +build drawsrv

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

// ============================================================
// GPU dumb buffer — allocates framebuffer from DRM device
// Falls back to /dev/fb0 if DRM unavailable.
// ============================================================

type FB struct {
	data  []byte
	w, h  int
	stride int
	mu    sync.RWMutex
}

func openFB(drmPath string) (*FB, error) {
	fd, err := syscall.Open(drmPath, syscall.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", drmPath, err)
	}

	// Create a dumb buffer
	// First, check if we can get mode info from the connector
	// DRM_IOCTL_MODE_GETCONNECTOR = 0xA07
	// For now: just try to map some buffers

	// Try to create a dumb buffer — this works without a mode set
	var create struct {
		w, h, bpp uint32
		flags     uint32
		handle    uint32
		pitch     uint32
		size      uint64
	}
	create.w = 1920
	create.h = 1080
	create.bpp = 32

	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), 0x40086402, uintptr(unsafe.Pointer(&create))); e != 0 {
		syscall.Close(fd)
		return nil, fmt.Errorf("DRM_IOCTL_MODE_CREATE_DUMB: %v", e)
	}

	// Map the dumb buffer
	var mp struct {
		handle uint32
		pad    uint32
		offset uint64
		size   uint64
		flags  uint64
	}
	mp.handle = create.handle
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), 0x40086407, uintptr(unsafe.Pointer(&mp))); e != 0 {
		syscall.Close(fd)
		return nil, fmt.Errorf("DRM_IOCTL_MODE_MAP_DUMB: %v", e)
	}

	data, err := syscall.Mmap(fd, int64(mp.offset), int(mp.size), syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("mmap drm: %w", err)
	}

	fb := &FB{
		data:   data,
		w:      int(create.w),
		h:      int(create.h),
		stride: int(create.pitch),
	}
	log.Printf("drm: %dx%d stride=%d handle=%d size=%d", fb.w, fb.h, fb.stride, create.handle, mp.size)
	return fb, nil
}

// openFB0 fallback — reads from /dev/fb0
func openFB0() (*FB, error) {
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
	f := &FB{w: int(vi.X), h: int(vi.Y)}
	f.stride = f.w * (int(vi.Bpp) / 8)
	log.Printf("fb0: %dx%d bpp=%d stride=%d", f.w, f.h, vi.Bpp, f.stride)
	f.data, err = syscall.Mmap(fd, 0, f.stride*f.h,
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
	if f.data != nil { syscall.Munmap(f.data) }
}

// ============================================================
// wsysmsg protocol
// ============================================================

const (
	Rerror = 1
	Trdmouse = 2; Rrdmouse = 3
	Tmoveto = 4; Rmoveto = 5
	Trdkbd = 10; Rrdkbd = 11
	Tbouncemouse = 8; Rbouncemouse = 9
	Tinit = 14; Rinit = 15
	Trddraw = 20; Rrddraw = 21
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
	p16(b, uint16(len(s))); copy(b[2:], s)
	return 2 + len(s)
}
func gstr(b []byte, o int) (int, string) {
	if o+2 > len(b) { return o, "" }
	n := int(g16(b[o:])); o += 2
	if o+n > len(b) { n = len(b) - o; if n < 0 { n = 0 } }
	return o + n, string(b[o:o+n])
}

func size(m *M) int {
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
	b[0], b[1] = m.T, m.Tag; n := 2
	switch m.T {
	case Rrdmouse:
		p32(b[n:], uint32(m.X)); n += 4
		p32(b[n:], uint32(m.Y)); n += 4
		p32(b[n:], uint32(m.B)); n += 4
		p32(b[n:], m.Ms); n += 4
		b[n] = 0; n++
	case Rrdkbd: p16(b[n:], uint16(m.R)); n += 2
	case Rrddraw:
		p32(b[n:], m.N); n += 4
		copy(b[n:], m.D[:m.N]); n += int(m.N)
	case Rerror: n += pstr(b[n:], m.Lbl)
	}
	return n
}

func wdec(b []byte, m *M) int {
	if len(b) < 2 { return -1 }
	m.T, m.Tag = b[0], b[1]; n := 2
	switch m.T {
	case Tinit:
		n, m.Ws = gstr(b, n)
		n, m.Lbl = gstr(b, n)
		_, _ = gstr(b, n)
	case Trdmouse, Trdkbd, Ttop:
	case Tmoveto:
		if n+8 <= len(b) { m.X = int32(g32(b[n:])); n += 4; m.Y = int32(g32(b[n:])); n += 4 }
	case Trddraw:
		if n+4 <= len(b) { m.N = g32(b[n:]); n += 4 }
	}
	return n
}

// ============================================================
// uinput mouse + keyboard
// ============================================================

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
	type ud struct { N [80]byte; ID struct{ B, Vr, Pd, Vn uint16 } }
	var d ud
	copy(d.N[:], "drawsrv-mouse")
	d.ID.B, d.ID.Vr, d.ID.Pd, d.ID.Vn = 3, 0x9f9f, 1, 1
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), 0x5503, uintptr(unsafe.Pointer(&d))); e != 0 { syscall.Close(fd); return nil, e }
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), 0x5501, 0); e != 0 { syscall.Close(fd); return nil, e }
	return &UI{fd: fd}, nil
}

type ie struct{ _ [16]byte; T, C uint16; V int32 }

func (u *UI) ev(t, c uint16, v int32) {
	e := ie{T: t, C: c, V: v}
	syscall.Syscall(syscall.SYS_WRITE, uintptr(u.fd), uintptr(unsafe.Pointer(&e)), unsafe.Sizeof(e))
}
func (u *UI) Move(x, y int32) { u.ev(3, 0, x); u.ev(3, 1, y); u.ev(0, 0, 0) }
func (u *UI) Btns(b int32) { u.ev(1, 0x110, b&1); u.ev(1, 0x111, (b>>1)&1); u.ev(1, 0x112, (b>>2)&1); u.ev(0, 0, 0) }
func (u *UI) Close() { if u.fd >= 0 { syscall.Syscall(syscall.SYS_IOCTL, uintptr(u.fd), 0x5502, 0); syscall.Close(u.fd) } }

// ============================================================
// Keyboard injection
// ============================================================

type Kbd struct{ fd int }

func openKbd() (*Kbd, error) {
	fd, err := syscall.Open("/dev/uinput", syscall.O_RDWR, 0)
	if err != nil { return nil, err }
	ub := func(i uintptr, bits ...int) {
		var b [64]uint32
		for _, v := range bits { b[v/32] |= 1 << (v % 32) }
		syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), i, uintptr(unsafe.Pointer(&b[0])))
	}
	ub(0x40045564, 1, 0)
	common := []int{1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17,18,19,20,21,22,23,24,25,26,27,28,29,30,31,32,33,34,35,36,37,38,39,40,41,42,43,44,45,46,47,48,49,50,51,52,53,54,55,56,57,58,97,100,102,103,104,105,106,107,108,109,110,111}
	for _, k := range common { ub(0x40045565, k) }
	type ud struct { N [80]byte; ID struct{ B, Vr, Pd, Vn uint16 } }
	var d ud
	copy(d.N[:], "drawsrv-kbd")
	d.ID.B, d.ID.Vr, d.ID.Pd, d.ID.Vn = 3, 0x9f9f, 2, 1
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), 0x5503, uintptr(unsafe.Pointer(&d))); e != 0 { syscall.Close(fd); return nil, e }
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), 0x5501, 0); e != 0 { syscall.Close(fd); return nil, e }
	return &Kbd{fd: fd}, nil
}

var p9k = map[int]int{
	1:1,2:2,3:3,4:4,5:5,6:6,7:7,8:8,9:9,10:10,11:11,12:12,13:13,14:14,15:15,
	16:16,17:17,18:18,19:19,20:20,21:21,22:22,23:23,24:24,25:25,
	26:26,27:27,28:28,29:29,30:30,31:31,32:32,33:33,34:34,
	35:35,36:36,37:37,38:38,39:39,40:40,41:41,42:42,43:43,
	44:44,45:45,46:46,47:47,48:48,49:49,50:50,51:51,52:52,53:53,
	54:54,55:55,56:56,57:57,58:58,97:97,100:100,102:102,103:103,
	104:104,105:105,106:106,107:107,108:108,109:109,110:110,111:111,
}

func (k *Kbd) Key(sc int) {
	if sc >= 0 && sc < 128 {
		if lk, ok := p9k[sc]; ok { k.ev(1, uint16(lk), 1) }
	} else if sc < 256 {
		if lk, ok := p9k[sc-128]; ok { k.ev(1, uint16(lk), 0) }
	}
}
func (k *Kbd) ev(t, c uint16, v int32) {
	e := ie{T: t, C: c, V: v}
	syscall.Syscall(syscall.SYS_WRITE, uintptr(k.fd), uintptr(unsafe.Pointer(&e)), unsafe.Sizeof(e))
	if t == 1 { k.sync() }
}
func (k *Kbd) sync() {
	e := ie{T: 0, C: 0, V: 0}
	syscall.Syscall(syscall.SYS_WRITE, uintptr(k.fd), uintptr(unsafe.Pointer(&e)), unsafe.Sizeof(e))
}
func (k *Kbd) Close() {
	if k.fd >= 0 { syscall.Syscall(syscall.SYS_IOCTL, uintptr(k.fd), 0x5502, 0); syscall.Close(k.fd) }
}

// ============================================================
// Protocol handlers
// ============================================================

func handleDraw(conn net.Conn, fb *FB, ui *UI) {
	defer conn.Close()
	h := make([]byte, 4)
	buf := make([]byte, 256*1024*1024)
	for {
		if _, err := conn.Read(h); err != nil { return }
		n := int(g32(h))
		if n < 2 || n > len(buf) { return }
		for t := 0; t < n; {
			r, err := conn.Read(buf[t:n])
			if err != nil { return }
			t += r
		}
		var m M
		wdec(buf[:n], &m)
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
				r.N = uint32(len(p)); r.D = p
			}
		case Tmoveto:
			if ui != nil { ui.Move(m.X, m.Y) }
			r.T, r.Tag = Rmoveto, m.Tag
		case Tbouncemouse:
			if ui != nil { ui.Btns(m.B) }
			r.T, r.Tag = Rbouncemouse, m.Tag
		default:
			r.T, r.Tag = Rerror, m.Tag
			r.Lbl = fmt.Sprintf("unhandled %d", m.T)
		}
		rs := size(&r)
		rb := make([]byte, rs+4)
		p32(rb, uint32(rs))
		wenc(&r, rb[4:])
		conn.Write(rb)
	}
}

func handleKbd(conn net.Conn, kbd *Kbd) {
	defer conn.Close()
	h := make([]byte, 4)
	for {
		if _, err := conn.Read(h); err != nil { return }
		sc := int(h[0]) | int(h[1])<<8 | int(h[2])<<16 | int(h[3])<<24
		kbd.Key(sc)
	}
}

// ============================================================
// Main
// ============================================================

func main() {
	tcpAddr := flag.String("tcp", "", "Draw protocol TCP (e.g. :17029)")
	kbdAddr := flag.String("kbd", "", "Keyboard scancode TCP (e.g. :17030)")
	drmDev := flag.String("drm", "/dev/dri/renderD129", "DRM render node for GPU buffer")
	flag.Parse()

	log.Printf("drawsrv starting")

	fb, err := openFB(*drmDev)
	if err != nil {
		log.Printf("DRM %s: %v, falling back to /dev/fb0", *drmDev, err)
		fb, err = openFB0()
		if err != nil {
			log.Printf("No fb0 either: %v, running headless", err)
		}
	}

	ui, _ := openUI()
	if ui != nil { defer ui.Close() }
	kbd, _ := openKbd()
	if kbd != nil { defer kbd.Close() }

	if *kbdAddr != "" && kbd != nil {
		go func() {
			ln, err := net.Listen("tcp", *kbdAddr)
			if err != nil { log.Fatal(err) }
			log.Printf("kbd on %s", *kbdAddr)
			for {
				c, err := ln.Accept()
				if err != nil { log.Fatal(err) }
				go handleKbd(c, kbd)
			}
		}()
	}

	if *tcpAddr != "" {
		ln, err := net.Listen("tcp", *tcpAddr)
		if err != nil { log.Fatal(err) }
		log.Printf("drawsrv on %s", *tcpAddr)
		for {
			c, err := ln.Accept()
			if err != nil { log.Fatal(err) }
			go handleDraw(c, fb, ui)
		}
	} else {
		sp := os.Getenv("DRAWSRV")
		if sp == "" {
			ns := os.Getenv("NAMESPACE")
			if ns == "" { ns = "/tmp" }
			sp = filepath.Join(ns, "drawsrv")
		}
		os.Remove(sp)
		ln, err := net.Listen("unix", sp)
		if err != nil { log.Fatal(err) }
		log.Printf("drawsrv on %s", sp)
		for {
			c, err := ln.Accept()
			if err != nil { log.Fatal(err) }
			go handleDraw(c, fb, ui)
		}
	}
}
