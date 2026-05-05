//go:build rcpud

package main

import (
	"log"
	"net"
	"os"
	"path/filepath"

	kclient "github.com/knusbaum/go9p/client"
	"github.com/knusbaum/go9p/fs"
	"github.com/knusbaum/go9p/proto"
)

// DrawProxy connects to the drawsrv Unix socket and adds
// /dev/draw and /dev/mouse proxy files to the namespace.
func AddDrawProxy(nsFS *fs.FS, nsRoot *fs.StaticDir, user string) {
	ns := os.Getenv("NAMESPACE")
	if ns == "" {
		log.Printf("drawproxy: NAMESPACE not set, skipping")
		return
	}
	sockPath := filepath.Join(ns, "drawsrv")
	if _, err := os.Stat(sockPath); err != nil {
		log.Printf("drawproxy: drawsrv not found at %s (%v), skipping", sockPath, err)
		return
	}

	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		log.Printf("drawproxy: dial drawsrv: %v", err)
		return
	}

	drawcl, err := kclient.NewClient(conn, user, "")
	if err != nil {
		log.Printf("drawproxy: 9P connect: %v", err)
		conn.Close()
		return
	}

	devDir := findOrCreateDevDir(nsFS, nsRoot, user)

	// Proxy /dev/draw — open once, keep the file handle
	if stat, err := drawcl.Stat("dev/draw"); err == nil {
		if f, err := drawcl.Open("dev/draw", proto.Ordwr); err == nil {
			devDir.AddChild(&DrawProxyFile{
				BaseFile: fs.NewBaseFile(stat),
				f:        f,
			})
			log.Printf("drawproxy: /dev/draw proxied")
		} else {
			log.Printf("drawproxy: open dev/draw: %v", err)
		}
	}

	// Proxy /dev/mouse — open once, keep the file handle
	if stat, err := drawcl.Stat("dev/mouse"); err == nil {
		if f, err := drawcl.Open("dev/mouse", proto.Oread); err == nil {
			devDir.AddChild(&MouseProxyFile{
				BaseFile: fs.NewBaseFile(stat),
				f:        f,
			})
			log.Printf("drawproxy: /dev/mouse proxied")
		} else {
			log.Printf("drawproxy: open dev/mouse: %v", err)
		}
	}

	log.Printf("drawproxy: synthetic graphics device connected")
}

func findOrCreateDevDir(fsys *fs.FS, root *fs.StaticDir, user string) *fs.StaticDir {
	for _, child := range root.Children() {
		if child.Stat().Name == "dev" {
			if d, ok := child.(*fs.StaticDir); ok {
				return d
			}
		}
	}
	d := fs.NewStaticDir(fsys.NewStat("dev", user, user, 0755|proto.DMDIR))
	root.AddChild(d)
	return d
}

// DrawProxyFile tunnels reads/writes to drawsrv's /dev/draw.
type DrawProxyFile struct {
	*fs.BaseFile
	f *kclient.File
}

func (f *DrawProxyFile) Read(fid uint64, offset uint64, count uint64) ([]byte, error) {
	buf := make([]byte, count)
	n, err := f.f.ReadAt(buf, int64(offset))
	if err != nil && n == 0 {
		return nil, nil
	}
	return buf[:n], nil
}

func (f *DrawProxyFile) Write(fid uint64, offset uint64, data []byte) (uint32, error) {
	n, err := f.f.WriteAt(data, int64(offset))
	return uint32(n), err
}

func (f *DrawProxyFile) Clunk(fid uint64) error {
	return nil
}

// MouseProxyFile tunnels reads to drawsrv's /dev/mouse.
type MouseProxyFile struct {
	*fs.BaseFile
	f *kclient.File
}

func (f *MouseProxyFile) Read(fid uint64, offset uint64, count uint64) ([]byte, error) {
	buf := make([]byte, count)
	n, err := f.f.Read(buf)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}

func (f *MouseProxyFile) Clunk(fid uint64) error {
	return nil
}
