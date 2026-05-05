package main

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/knusbaum/go9p"
	"github.com/knusbaum/go9p/client"
	"github.com/knusbaum/go9p/fs"
	"github.com/knusbaum/go9p/proto"
)

type mockFBData struct {
	data []byte
}

func (m *mockFBData) Copy() []byte {
	p := make([]byte, len(m.data))
	copy(p, m.data)
	return p
}

func serveFS(fsys *fs.FS, ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go go9p.ServeReadWriter(c, c, fsys.Server())
	}
}

func TestAddFramebuffer(t *testing.T) {
	user := "scott"
	fsys, root := fs.NewFS(user, user, 0755)
	devDir := fs.NewStaticDir(fsys.NewStat("dev", user, user, 0755|proto.DMDIR))
	root.AddChild(devDir)

	fb := &mockFBData{data: make([]byte, 640*480*4)}
	for i := range fb.data {
		fb.data[i] = byte(i % 256)
	}

	addFramebuffer(fsys, root, user, fb)

	dev := root.Children()["dev"]
	if dev == nil {
		t.Fatal("/dev not found")
	}
	devDir2 := dev.(*fs.StaticDir)
	fbDir := devDir2.Children()["fb"]
	if fbDir == nil {
		t.Fatal("/dev/fb not found")
	}
	_ = fbDir

	// Serve over unix socket and verify via 9P client
	sockDir, _ := os.MkdirTemp("", "fb-test.*")
	defer os.RemoveAll(sockDir)
	sockPath := filepath.Join(sockDir, "test.sock")
	ln, _ := net.Listen("unix", sockPath)
	defer ln.Close()
	go serveFS(fsys, ln)
	time.Sleep(50 * time.Millisecond)

	conn, _ := net.Dial("unix", sockPath)
	defer conn.Close()
	cl, _ := client.NewClient(conn, user, "")
	f, err := cl.Open("dev/fb/data", proto.Oread)
	if err != nil {
		t.Fatalf("open dev/fb/data: %v", err)
	}
	data := make([]byte, 32)
	n, err := f.ReadAt(data, 0)
	if err != nil && err.Error() != "EOF" {
		t.Fatalf("read: %v", err)
	}
	if n != 32 {
		t.Fatalf("expected 32 bytes, got %d", n)
	}
	for i := 0; i < 32; i++ {
		if data[i] != byte(i%256) {
			t.Fatalf("byte %d: got %d, want %d", i, data[i], byte(i%256))
		}
	}
}

func TestFramebufferIntegration(t *testing.T) {
	user := "scott"
	fsys, root := fs.NewFS(user, user, 0755)
	devDir := fs.NewStaticDir(fsys.NewStat("dev", user, user, 0755|proto.DMDIR))
	root.AddChild(devDir)

	fb := &mockFBData{data: make([]byte, 64*64*4)}
	for i := 0; i < len(fb.data); i += 4 {
		fb.data[i] = 0xFF
		fb.data[i+1] = 0x00
		fb.data[i+2] = 0x00
		fb.data[i+3] = 0xFF
	}
	addFramebuffer(fsys, root, user, fb)

	sockDir, _ := os.MkdirTemp("", "fb-int-test.*")
	defer os.RemoveAll(sockDir)
	sockPath := filepath.Join(sockDir, "test.sock")
	ln, _ := net.Listen("unix", sockPath)
	defer ln.Close()
	go serveFS(fsys, ln)
	time.Sleep(50 * time.Millisecond)

	conn, _ := net.Dial("unix", sockPath)
	defer conn.Close()
	cl, _ := client.NewClient(conn, user, "")

	s, err := cl.Stat("dev/fb/data")
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if s.Length != uint64(len(fb.data)) {
		t.Errorf("stat.Length=%d, expected %d", s.Length, len(fb.data))
	}

	f, _ := cl.Open("dev/fb/data", proto.Oread)
	data := make([]byte, len(fb.data))
	n, _ := f.ReadAt(data, 0)
	if n == 0 {
		t.Fatal("read returned 0 bytes")
	}
	if data[0] != 0xFF || data[1] != 0x00 || data[2] != 0x00 || data[3] != 0xFF {
		t.Errorf("first pixel wrong: %x %x %x %x", data[0], data[1], data[2], data[3])
	}
}

func TestFramebufferOffsetRead(t *testing.T) {
	user := "scott"
	fsys, root := fs.NewFS(user, user, 0755)
	devDir := fs.NewStaticDir(fsys.NewStat("dev", user, user, 0755|proto.DMDIR))
	root.AddChild(devDir)

	fb := &mockFBData{data: make([]byte, 100)}
	for i := range fb.data {
		fb.data[i] = byte(i)
	}
	addFramebuffer(fsys, root, user, fb)

	sockDir, _ := os.MkdirTemp("", "fb-off-test.*")
	defer os.RemoveAll(sockDir)
	sockPath := filepath.Join(sockDir, "test.sock")
	ln, _ := net.Listen("unix", sockPath)
	defer ln.Close()
	go serveFS(fsys, ln)
	time.Sleep(50 * time.Millisecond)

	conn, _ := net.Dial("unix", sockPath)
	defer conn.Close()
	cl, _ := client.NewClient(conn, user, "")
	f, _ := cl.Open("dev/fb/data", proto.Oread)

	data := make([]byte, 10)
	n, _ := f.ReadAt(data, 50)
	if n != 10 {
		t.Fatalf("expected 10 bytes, got %d", n)
	}
	for i := 0; i < 10; i++ {
		if data[i] != byte(50+i) {
			t.Fatalf("offset byte %d: got %d, want %d", i, data[i], 50+i)
		}
	}
}

func TestFramebufferInNamespaceOnlyIfDrawsrvSet(t *testing.T) {
	oldAddr := drawsrvAddr
	defer func() { drawsrvAddr = oldAddr }()

	user := "scott"
	s, c := net.Pipe()
	defer s.Close()
	defer c.Close()

	fsys, root := fs.NewFS(user, user, 0755)
	devDir := fs.NewStaticDir(fsys.NewStat("dev", user, user, 0755|proto.DMDIR))
	root.AddChild(devDir)
	consFile := fs.NewStaticFile(fsys.NewStat("cons", user, user, 0666), []byte(""))
	devDir.AddChild(consFile)
	go go9p.ServeReadWriter(s, s, fsys.Server())

	drawsrvAddr = ""
	cl, _ := client.NewClient(c, user, "")
	_, nsRoot := setupNamespace(cl, user)

	dev := nsRoot.Children()["dev"]
	if dev != nil {
		if d, ok := dev.(*fs.StaticDir); ok {
			if d.Children()["fb"] != nil {
				t.Error("/dev/fb should not exist when drawsrvAddr is empty")
			}
		}
	}
}

func TestFramebufferAlongsideCons(t *testing.T) {
	oldAddr := drawsrvAddr
	defer func() { drawsrvAddr = oldAddr }()

	drawsrvAddr = ":17029"
	user := "scott"
	s, c := net.Pipe()
	defer s.Close()
	defer c.Close()

	fsys, root := fs.NewFS(user, user, 0755)
	devDir := fs.NewStaticDir(fsys.NewStat("dev", user, user, 0755|proto.DMDIR))
	root.AddChild(devDir)
	consFile := fs.NewStaticFile(fsys.NewStat("cons", user, user, 0666), []byte(""))
	devDir.AddChild(consFile)
	go go9p.ServeReadWriter(s, s, fsys.Server())

	cl, _ := client.NewClient(c, user, "")
	_, nsRoot := setupNamespace(cl, user)
	dev := nsRoot.Children()["dev"].(*fs.StaticDir)

	if dev.Children()["cons"] == nil {
		t.Error("cons should still exist alongside fb")
	}
}

func TestFBFileRead(t *testing.T) {
	fb := &mockFBData{data: make([]byte, 100)}
	for i := range fb.data {
		fb.data[i] = byte(i)
	}
	ff := &FBFile{fb: fb, fbLen: 100}
	pix := ff.fb.Copy()
	if len(pix) != 100 {
		t.Fatalf("expected 100, got %d", len(pix))
	}
	if pix[0] != 0 || pix[99] != 99 {
		t.Errorf("data mismatch: first=%d last=%d", pix[0], pix[99])
	}
}
