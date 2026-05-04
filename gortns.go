//go:build rcpud

package main

import (
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/knusbaum/go9p/fs"
	"github.com/knusbaum/go9p/proto"
)

// GortnManager manages the /gortns/ directory and all agent lifecycles.
type GortnManager struct {
	mu       sync.Mutex
	agents   map[string]*Gortn
	baseDir  *fs.StaticDir
	fsys     *fs.FS
	user     string
}

// NewGortnManager creates a /gortns/ directory on the given filesystem.
func NewGortnManager(fsys *fs.FS, root *fs.StaticDir, user string) *GortnManager {
	gm := &GortnManager{
		agents:  make(map[string]*Gortn),
		fsys:    fsys,
		user:    user,
	}
	gm.baseDir = fs.NewStaticDir(fsys.NewStat("gortns", user, user, 0755|proto.DMDIR))
	root.AddChild(gm.baseDir)

	// Add the gortns ctl file at /gortns/ctl for creating agents
	gm.baseDir.AddChild(&GortnDirCtl{gm: gm, BaseFile: fs.NewBaseFile(fsys.NewStat("ctl", user, user, 0222))})

	log.Printf("Gortn manager initialized at /gortns/")
	return gm
}

// Spawn creates a new agent directory under /gortns/<id>/
func (gm *GortnManager) Spawn(id, thought string) error {
	gm.mu.Lock()
	defer gm.mu.Unlock()

	if _, exists := gm.agents[id]; exists {
		return fmt.Errorf("agent %s already exists", id)
	}

	g := newGortn(gm, id, thought)
	gm.agents[id] = g

	dir := fs.NewStaticDir(gm.fsys.NewStat(id, gm.user, gm.user, 0755|proto.DMDIR))
	gm.baseDir.AddChild(dir)

	dir.AddChild(&GortnStatusFile{g: g, BaseFile: fs.NewBaseFile(gm.fsys.NewStat("status", gm.user, gm.user, 0444))})
	dir.AddChild(&GortnCtlFile{g: g, BaseFile: fs.NewBaseFile(gm.fsys.NewStat("ctl", gm.user, gm.user, 0222))})
	dir.AddChild(&GortnInFile{g: g, BaseFile: fs.NewBaseFile(gm.fsys.NewStat("in", gm.user, gm.user, 0222))})
	dir.AddChild(&GortnOutFile{g: g, BaseFile: fs.NewBaseFile(gm.fsys.NewStat("out", gm.user, gm.user, 0444))})

	go g.run()
	return nil
}

// --- Agent struct and lifecycle ---

type GortnState string

const (
	StateRunning GortnState = "running"
	StateStopped GortnState = "stopped"
)

type Gortn struct {
	id      string
	thought string
	state   GortnState
	in      chan []byte
	out     chan []byte
	ctl     chan string
	stop    chan struct{}
}

func newGortn(gm *GortnManager, id, thought string) *Gortn {
	return &Gortn{
		id:      id,
		thought: thought,
		state:   StateRunning,
		in:      make(chan []byte, 64),
		out:     make(chan []byte, 64),
		ctl:     make(chan string, 8),
		stop:    make(chan struct{}),
	}
}

func (g *Gortn) run() {
	log.Printf("[gortn %s] started: %s", g.id, g.thought)
	g.out <- []byte(fmt.Sprintf("[gortn %s] %s\n", g.id, g.thought))

	for {
		select {
		case data := <-g.in:
			g.out <- append([]byte("echo: "), data...)

		case cmd := <-g.ctl:
			cmd = strings.TrimSpace(cmd)
			switch {
			case cmd == "stop":
				g.state = StateStopped
				g.out <- []byte(fmt.Sprintf("[gortn %s] stopped\n", g.id))
				return
			case strings.HasPrefix(cmd, "spawn "):
				// Sub-agent spawn—pass thought through
				subThought := strings.TrimPrefix(cmd, "spawn ")
				g.out <- []byte(fmt.Sprintf("[gortn %s] sub-thought: %s\n", g.id, subThought))
			default:
				g.out <- []byte(fmt.Sprintf("[gortn %s] unknown ctl: %s\n", g.id, cmd))
			}

		case <-g.stop:
			g.state = StateStopped
			return
		}
	}
}

func (g *Gortn) stopAgent() {
	select {
	case g.stop <- struct{}{}:
	default:
	}
}

// --- 9P files ---

// GortnDirCtl handles writes to /gortns/ctl
type GortnDirCtl struct {
	*fs.BaseFile
	gm *GortnManager
}

func (f *GortnDirCtl) Write(fid uint64, offset uint64, data []byte) (uint32, error) {
	cmd := strings.TrimSpace(string(data))
	if strings.HasPrefix(cmd, "spawn ") {
		thought := strings.TrimPrefix(cmd, "spawn ")
		id := fmt.Sprintf("t%d", time.Now().UnixNano())
		if err := f.gm.Spawn(id, thought); err != nil {
			return 0, err
		}
		return uint32(len(data)), nil
	}
	return uint32(len(data)), nil
}

// GortnStatusFile handles reads from /gortns/<id>/status
type GortnStatusFile struct {
	*fs.BaseFile
	g *Gortn
}

func (f *GortnStatusFile) Read(fid uint64, offset uint64, count uint64) ([]byte, error) {
	data := []byte(string(f.g.state) + "\n")
	if offset >= uint64(len(data)) {
		return nil, io.EOF
	}
	end := offset + count
	if end > uint64(len(data)) {
		end = uint64(len(data))
	}
	return data[offset:end], nil
}

// GortnCtlFile handles writes to /gortns/<id>/ctl
type GortnCtlFile struct {
	*fs.BaseFile
	g *Gortn
}

func (f *GortnCtlFile) Write(fid uint64, offset uint64, data []byte) (uint32, error) {
	f.g.ctl <- strings.TrimSpace(string(data))
	return uint32(len(data)), nil
}

// GortnInFile handles writes to /gortns/<id>/in
type GortnInFile struct {
	*fs.BaseFile
	g *Gortn
}

func (f *GortnInFile) Write(fid uint64, offset uint64, data []byte) (uint32, error) {
	select {
	case f.g.in <- data:
	case <-time.After(time.Second):
		// buffer full, drop
	}
	return uint32(len(data)), nil
}

// GortnOutFile handles reads from /gortns/<id>/out
type GortnOutFile struct {
	*fs.BaseFile
	g *Gortn
}

func (f *GortnOutFile) Read(fid uint64, offset uint64, count uint64) ([]byte, error) {
	select {
	case data := <-f.g.out:
		if uint64(len(data)) > count {
			return data[:count], nil
		}
		return data, nil
	case <-time.After(100 * time.Millisecond):
		return nil, nil
	}
}
