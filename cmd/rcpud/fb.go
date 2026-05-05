package main

import (
	"fmt"
	"log"
	"sync"
	"syscall"
	"unsafe"

	"github.com/knusbaum/go9p/fs"
	"github.com/knusbaum/go9p/proto"
)

// Copyer is the interface the framebuffer tests mock.
type Copyer interface {
	Copy() []byte
}

// mockFB (defined in fb_test.go) also satisfies Copyer.

// openFBDumb and openFB0 are package-level variables so tests can mock them.
var openFBDumb func() Copyer
var openFB0 func() Copyer

func init() {
	openFBDumb = func() Copyer { return openFBDumbImpl() }
	openFB0 = func() Copyer { return openFB0Impl() }
}

// framebuffer from DRM or /dev/fb0
type framebuffer struct {
	data   []byte
	w, h, stride int
	mu     sync.RWMutex
	fd     int
}

func (f *framebuffer) Copy() []byte {
	f.mu.RLock()
	defer f.mu.RUnlock()
	p := make([]byte, len(f.data))
	copy(p, f.data)
	return p
}

func openFBDumbImpl() Copyer {
	fd, err := syscall.Open("/dev/dri/renderD129", syscall.O_RDWR, 0)
	if err != nil {
		return nil
	}
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
		return nil
	}
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
		return nil
	}
	data, err := syscall.Mmap(fd, int64(mp.offset), int(mp.size),
		syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		syscall.Close(fd)
		return nil
	}
	log.Printf("DRM framebuffer: %dx%d", create.w, create.h)
	return &framebuffer{
		data:   data,
		w:      int(create.w),
		h:      int(create.h),
		stride: int(create.pitch),
		fd:     fd,
	}
}

func openFB0Impl() Copyer {
	fd, err := syscall.Open("/dev/fb0", syscall.O_RDWR, 0)
	if err != nil {
		return nil
	}
	var vi struct {
		X, Y, Xv, Yv, Xo, Yo, Bpp uint32
		_ [44]byte
	}
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), 0x4600, uintptr(unsafe.Pointer(&vi))); e != 0 {
		syscall.Close(fd)
		return nil
	}
	stride := int(vi.X) * (int(vi.Bpp) / 8)
	size := stride * int(vi.Y)
	data, err := syscall.Mmap(fd, 0, size, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		syscall.Close(fd)
		return nil
	}
	log.Printf("fb0 framebuffer: %dx%d", vi.X, vi.Y)
	return &framebuffer{
		data:   data,
		w:      int(vi.X),
		h:      int(vi.Y),
		stride: stride,
		fd:     fd,
	}
}

// FBFile serves framebuffer pixels through 9P.
type FBFile struct {
	*fs.BaseFile
	fb             Copyer
	fbLen          uint64
}

func (f *FBFile) Open(fid uint64, omode proto.Mode) error {
	return nil
}

func (f *FBFile) Read(fid uint64, offset uint64, count uint64) ([]byte, error) {
	pix := f.fb.Copy()
	if offset >= uint64(len(pix)) {
		return nil, fmt.Errorf("EOF")
	}
	end := offset + count
	if end > uint64(len(pix)) {
		end = uint64(len(pix))
	}
	return pix[offset:end], nil
}

func (f *FBFile) Close(fid uint64) error {
	return nil
}

// addFramebuffer adds /dev/fb/data to the namespace.
// fb may be nil (no framebuffer available).
func addFramebuffer(fsys *fs.FS, root *fs.StaticDir, user string, fb Copyer) {
	if fb == nil {
		log.Printf("No framebuffer available, skipping /dev/fb")
		return
	}
	children := root.Children()
	devRaw := children["dev"]
	var devDir *fs.StaticDir
	if d, ok := devRaw.(*fs.StaticDir); ok {
		devDir = d
	} else {
		devDir = fs.NewStaticDir(fsys.NewStat("dev", user, user, 0755|proto.DMDIR))
		root.AddChild(devDir)
	}

	fbDir := fs.NewStaticDir(fsys.NewStat("fb", user, user, 0755|proto.DMDIR))

	// Read actual data for stat size
	pix := fb.Copy()
	stat := fsys.NewStat("data", user, user, 0444)
	stat.Length = uint64(len(pix))

	fbDir.AddChild(&FBFile{
		BaseFile: fs.NewBaseFile(stat),
		fb:       fb,
	})
	devDir.AddChild(fbDir)
	log.Printf("Framebuffer /dev/fb/data (%d bytes)", len(pix))
}

var _ Copyer = (*framebuffer)(nil) // framebuffer satisfies Copyer
