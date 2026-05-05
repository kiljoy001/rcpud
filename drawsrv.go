//go:build drawsrv

package main

import (
	"encoding/binary"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"sync"

	"github.com/knusbaum/go9p"
	"github.com/knusbaum/go9p/fs"
	"github.com/knusbaum/go9p/proto"
)

/*
 * drawsrv - Plan 9 draw protocol server for Linux
 *
 * Serves /dev/draw, /dev/mouse, /dev/cursor, /dev/kbd via 9P.
 * Run alongside rcpud to support graphical CPU sessions (rcpu -G).
 *
 * Protocol:
 *   /dev/draw new      → write channel string, read back id like "dev/draw/0"
 *   /dev/draw/N/ctl    → window control (init, resize, label, top, etc.)
 *   /dev/draw/N/data   → pixel data read/write (wsysmsg format)
 *   /dev/mouse         → blocking read of mouse events (m9 format)
 *   /dev/cursor        → write cursor image
 *   /dev/kbd           → blocking read of keyboard events
 *
 * The draw messages use the wsysmsg protocol defined in drawfcall.h.
 */

// Wsysmsg types from drawfcall.h
const (
	Rerror   = 1
	Trdmouse = 2
	Rrdmouse = 3
	Tmoveto  = 4
	Rmoveto  = 5
	Tcursor  = 6
	Rcursor  = 7
	Tbouncemouse = 8
	Rbouncemouse = 9
	Trdkbd   = 10
	Rrdkbd   = 11
	Tlabel   = 12
	Rlabel   = 13
	Tinit    = 14
	Rinit    = 15
	Trdsnarf = 16
	Rrdsnarf = 17
	Twrsnarf = 18
	Rwrsnarf = 19
	Trddraw  = 20
	Rrddraw  = 21
	Twrdraw  = 22
	Rwrdraw  = 23
	Ttop     = 24
	Rtop     = 25
	Tresize  = 26
	Rresize  = 27
	Tcursor2 = 28
	Rcursor2 = 29
	Tctxt    = 30
	Rctxt    = 31
	Trdkbd4  = 32
	Rrdkbd4  = 33
)

// DrawWindow represents a rio window or off-screen image
type DrawWindow struct {
	ID      int
	Rect    Rectangle
	Data    []byte // pixel data, RGBA
	Label   string
	Visible bool
	mu      sync.Mutex
}

type Rectangle struct {
	MinX, MinY, MaxX, MaxY int
}

func (r Rectangle) Width() int  { return r.MaxX - r.MinX }
func (r Rectangle) Height() int { return r.MaxY - r.MinY }
func (r Rectangle) Size() int   { return r.Width() * r.Height() * 4 }

func newDrawWindow(id int, rect Rectangle) *DrawWindow {
	w := rect.Width()
	h := rect.Height()
	if w <= 0 {
		w = 640
	}
	if h <= 0 {
		h = 480
	}
	return &DrawWindow{
		ID:    id,
		Rect:  Rectangle{0, 0, w, h},
		Data:  make([]byte, w*h*4),
		Label: "drawsrv",
	}
}

// DrawSession manages one draw context (one rcpu -G connection)
type DrawSession struct {
	mu       sync.Mutex
	nextWin  int
	windows  map[int]*DrawWindow
}

func NewDrawSession() *DrawSession {
	return &DrawSession{
		windows: make(map[int]*DrawWindow),
	}
}

func (s *DrawSession) NewWindow(rect Rectangle) *DrawWindow {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.nextWin
	s.nextWin++
	w := newDrawWindow(id, rect)
	s.windows[id] = w
	return w
}

func (s *DrawSession) GetWindow(id int) *DrawWindow {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.windows[id]
}

// --- 9P filesystem setup ---

type drawSrv struct {
	listener net.Listener
	session  *DrawSession
	root     *fs.StaticDir
	fs       *fs.FS
}

func newDrawSrv(socketPath string) (*drawSrv, error) {
	if err := os.MkdirAll(filepath.Dir(socketPath), 0755); err != nil {
		return nil, fmt.Errorf("mkdir: %w", err)
	}
	os.Remove(socketPath) // remove stale socket

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}

	d := &drawSrv{
		listener: ln,
		session:  NewDrawSession(),
	}

	// Build the 9P tree
	fsys, root := fs.NewFS("drawsrv", "drawsrv", 0755)
	d.fs = fsys
	d.root = root

	// /dev
	devDir := fs.NewStaticDir(fsys.NewStat("dev", "drawsrv", "drawsrv", 0755|proto.DMDIR))

	// /dev/draw
	drawDir := fs.NewStaticDir(fsys.NewStat("draw", "drawsrv", "drawsrv", 0755|proto.DMDIR))

	// /dev/draw/new — write channel desc, read back "dev/draw/N"
	drawDir.AddChild(fs.NewStaticFile(fsys.NewStat("new", "drawsrv", "drawsrv", 0444),
		[]byte("dev/draw/0\n")))

	// /dev/draw/0 — a window directory
	win0 := fs.NewStaticDir(fsys.NewStat("0", "drawsrv", "drawsrv", 0755|proto.DMDIR))
	win0.AddChild(fs.NewStaticFile(fsys.NewStat("ctl", "drawsrv", "drawsrv", 0444),
		[]byte("")))
	win0.AddChild(fs.NewStaticFile(fsys.NewStat("data", "drawsrv", "drawsrv", 0444),
		[]byte("")))
	drawDir.AddChild(win0)

	devDir.AddChild(drawDir)
	root.AddChild(devDir)

	log.Printf("drawsrv ready at %s", socketPath)
	return d, nil
}

func (d *drawSrv) Serve() {
	for {
		conn, err := d.listener.Accept()
		if err != nil {
			log.Printf("accept: %v", err)
			return
		}
		go go9p.ServeReadWriter(conn, conn, d.fs.Server())
	}
}

func main() {
	socketPath := os.Getenv("DRAWSRV")
	if socketPath == "" {
		ns := os.Getenv("NAMESPACE")
		if ns == "" {
			ns = "/tmp"
		}
		socketPath = filepath.Join(ns, "drawsrv")
	}

	srv, err := newDrawSrv(socketPath)
	if err != nil {
		log.Fatalf("drawsrv: %v", err)
	}

	log.Printf("drawsrv listening on %s", socketPath)
	srv.Serve()
}

// --- Helper: big-endian uint32 read/write (Plan 9 wire format) ---

func gbit32(b []byte) uint32 {
	return binary.BigEndian.Uint32(b)
}

func pbit32(b []byte, v uint32) {
	binary.BigEndian.PutUint32(b, v)
}
