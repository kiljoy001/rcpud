package main

import (
	"encoding/binary"
	"fmt"
	"image"
	"image/draw"
	"log"
	"net"
	"os"
	"sync"
	"time"
	"unsafe"

	"github.com/knusbaum/go9p"
	"github.com/knusbaum/go9p/fs"
	"github.com/knusbaum/go9p/proto"
	"github.com/veandco/go-sdl2/sdl"
)

/* 
 * drawsrv.go - The o9 Synthetic Graphics Device
 * Implements /dev/draw and /dev/mouse using SDL2.
 */

type Image struct {
	id   uint32
	rgba *image.RGBA
}

type DrawSrv struct {
	mu     sync.Mutex
	images map[uint32]*Image
	width  int
	height int
	user   string
	
	window   *sdl.Window
	renderer *sdl.Renderer
	texture  *sdl.Texture
	
	mouseMsg chan string
}

func NewDrawSrv(user string, width, height int) *DrawSrv {
	ds := &DrawSrv{
		images:   make(map[uint32]*Image),
		width:    width,
		height:   height,
		user:     user,
		mouseMsg: make(chan string, 100),
	}
	
	ds.images[0] = &Image{
		id:   0,
		rgba: image.NewRGBA(image.Rect(0, 0, width, height)),
	}

	return ds
}

func (ds *DrawSrv) initSDL() {
	if err := sdl.Init(sdl.INIT_EVERYTHING); err != nil {
		log.Printf("SDL Init failed: %v", err)
		return
	}

	window, err := sdl.CreateWindow("o9 Synthetic Display", sdl.WINDOWPOS_UNDEFINED, sdl.WINDOWPOS_UNDEFINED,
		int32(ds.width), int32(ds.height), sdl.WINDOW_SHOWN|sdl.WINDOW_RESIZABLE)
	if err != nil {
		log.Printf("Window creation failed: %v", err)
		return
	}
	ds.window = window

	renderer, err := sdl.CreateRenderer(window, -1, sdl.RENDERER_ACCELERATED)
	if err != nil {
		log.Printf("Renderer creation failed: %v", err)
		return
	}
	ds.renderer = renderer

	texture, err := renderer.CreateTexture(sdl.PIXELFORMAT_ABGR8888, sdl.TEXTUREACCESS_STREAMING, int32(ds.width), int32(ds.height))
	if err != nil {
		log.Printf("Texture creation failed: %v", err)
		return
	}
	ds.texture = texture

	go ds.eventLoop()
}

func (ds *DrawSrv) eventLoop() {
	for {
		event := sdl.WaitEvent()
		switch t := event.(type) {
		case *sdl.QuitEvent:
			os.Exit(0)
		case *sdl.MouseMotionEvent:
			msg := fmt.Sprintf("m%11d %11d %11d %11d", t.X, t.Y, 0, uint32(time.Now().UnixMilli()))
			select {
			case ds.mouseMsg <- msg:
			default:
			}
		}
	}
}

func (ds *DrawSrv) refresh() {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	
	img := ds.images[0].rgba
	ds.texture.Update(nil, unsafe.Pointer(&img.Pix[0]), img.Stride)
	ds.renderer.Clear()
	ds.renderer.Copy(ds.texture, nil, nil)
	ds.renderer.Present()
}

type DrawFile struct {
	*fs.BaseFile
	ds *DrawSrv
}

func (f *DrawFile) Read(fid uint64, offset uint64, count uint64) ([]byte, error) {
	info := fmt.Sprintf("%11d %11d %11d %-11s %11d %11d %11d %11d %11d ",
		0, 0, 0, "r8g8b8", 0, 0, f.ds.width, f.ds.height, 0)
	if offset >= uint64(len(info)) { return nil, nil }
	return []byte(info[offset:]), nil
}

func (f *DrawFile) Write(fid uint64, offset uint64, data []byte) (uint32, error) {
	f.ds.mu.Lock()
	originalLen := uint32(len(data))
	
	for len(data) > 0 {
		cmd := data[0]
		switch cmd {
		case 'b':
			if len(data) < 60 { goto done }
			id := binary.LittleEndian.Uint32(data[1:5])
			r := readRect(data[15:31])
			f.ds.images[id] = &Image{id: id, rgba: image.NewRGBA(r)}
			data = data[60:]
		case 'd':
			if len(data) < 45 { goto done }
			dstid := binary.LittleEndian.Uint32(data[1:5])
			srcid := binary.LittleEndian.Uint32(data[5:9])
			maskid := binary.LittleEndian.Uint32(data[9:13])
			r := readRect(data[13:29])
			p0 := readPoint(data[29:37])
			p1 := readPoint(data[37:45])
			f.ds.drawOp(dstid, srcid, maskid, r, p0, p1)
			data = data[45:]
		default:
			data = nil
		}
	}
done:
	f.ds.mu.Unlock()
	if f.ds.renderer != nil { f.ds.refresh() }
	return originalLen, nil
}

func (ds *DrawSrv) drawOp(dstid, srcid, maskid uint32, r image.Rectangle, p0, p1 image.Point) {
	dst := ds.images[dstid]
	src := ds.images[srcid]
	mask := ds.images[maskid]
	if dst == nil || src == nil { return }
	
	if mask == nil {
		draw.Draw(dst.rgba, r, src.rgba, p0, draw.Over)
	} else {
		draw.DrawMask(dst.rgba, r, src.rgba, p0, mask.rgba, p1, draw.Over)
	}
}

type MouseFile struct {
	*fs.BaseFile
	ds *DrawSrv
}

func (f *MouseFile) Read(fid uint64, offset uint64, count uint64) ([]byte, error) {
	msg := <- f.ds.mouseMsg
	return []byte(msg), nil
}

func readRect(b []byte) image.Rectangle {
	return image.Rect(
		int(int32(binary.LittleEndian.Uint32(b[0:4]))),
		int(int32(binary.LittleEndian.Uint32(b[4:8]))),
		int(int32(binary.LittleEndian.Uint32(b[8:12]))),
		int(int32(binary.LittleEndian.Uint32(b[12:16]))),
	)
}

func readPoint(b []byte) image.Point {
	return image.Pt(
		int(int32(binary.LittleEndian.Uint32(b[0:4]))),
		int(int32(binary.LittleEndian.Uint32(b[4:8]))),
	)
}

func main() {
	user := os.Getenv("USER")
	if user == "" { user = "scott" }
	ds := NewDrawSrv(user, 1024, 768)
	ds.initSDL()

	fsys, root := fs.NewFS(user, user, 0755)
	devDir := fs.NewStaticDir(fsys.NewStat("dev", user, user, 0755|proto.DMDIR))
	root.AddChild(devDir)
	
	devDir.AddChild(&DrawFile{
		BaseFile: fs.NewBaseFile(fsys.NewStat("draw", user, user, 0666)),
		ds:       ds,
	})
	devDir.AddChild(&MouseFile{
		BaseFile: fs.NewBaseFile(fsys.NewStat("mouse", user, user, 0444)),
		ds:       ds,
	})

	ns := os.Getenv("NAMESPACE")
	if ns == "" { log.Fatal("NAMESPACE not set") }
	sockPath := ns + "/drawsrv"
	os.Remove(sockPath)
	ln, err := net.Listen("unix", sockPath)
	if err != nil { log.Fatal(err) }
	
	fmt.Printf("o9 Synthetic Draw Server online at %s\n", sockPath)
	for {
		c, err := ln.Accept()
		if err != nil { continue }
		go go9p.ServeReadWriter(c, c, fsys.Server())
	}
}
