// Package fb provides framebuffer capture from Linux DRM or /dev/fb0.
// Both rcpud and drawsrv use this to serve pixels over 9P or wsysmsg.
package fb

import (
	"fmt"
	"sync"
	"syscall"
	"unsafe"
)

// Buf is a memory-mapped framebuffer.
type Buf struct {
	Data   []byte
	Width  int
	Height int
	Stride int
	mu     sync.RWMutex
	fd     int
}

// OpenDRM opens a DRM render node and allocates a dumb buffer.
func OpenDRM(path string) (*Buf, error) {
	fd, err := syscall.Open(path, syscall.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
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
		return nil, fmt.Errorf("DRM_CREATE_DUMB: %v", e)
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
		return nil, fmt.Errorf("DRM_MAP_DUMB: %v", e)
	}
	data, err := syscall.Mmap(fd, int64(mp.offset), int(mp.size),
		syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("mmap drm: %w", err)
	}
	return &Buf{
		Data:   data,
		Width:  int(create.w),
		Height: int(create.h),
		Stride: int(create.pitch),
		fd:     fd,
	}, nil
}

// OpenFB0 opens /dev/fb0 and mmaps it.
func OpenFB0() (*Buf, error) {
	fd, err := syscall.Open("/dev/fb0", syscall.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open fb0: %w", err)
	}
	var vi struct {
		X, Y, Xv, Yv, Xo, Yo, Bpp uint32
		_ [44]byte
	}
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), 0x4600, uintptr(unsafe.Pointer(&vi))); e != 0 {
		syscall.Close(fd)
		return nil, fmt.Errorf("FBIOGET_VSCREENINFO: %v", e)
	}
	stride := int(vi.X) * (int(vi.Bpp) / 8)
	size := stride * int(vi.Y)
	data, err := syscall.Mmap(fd, 0, size, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("mmap fb0: %w", err)
	}
	return &Buf{
		Data:   data,
		Width:  int(vi.X),
		Height: int(vi.Y),
		Stride: stride,
		fd:     fd,
	}, nil
}

// Copy returns a copy of the current framebuffer pixels.
func (b *Buf) Copy() []byte {
	b.mu.RLock()
	defer b.mu.RUnlock()
	p := make([]byte, len(b.Data))
	copy(p, b.Data)
	return p
}

// Close unmaps and closes the framebuffer.
func (b *Buf) Close() {
	if b.Data != nil {
		syscall.Munmap(b.Data)
	}
	if b.fd >= 0 {
		syscall.Close(b.fd)
	}
}
