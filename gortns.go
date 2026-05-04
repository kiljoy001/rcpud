//go:build rcpud

package main

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/knusbaum/go9p/fs"
	"github.com/knusbaum/go9p/proto"
)

// GortnManager manages the /gortns/ directory and all agent lifecycles.
type GortnManager struct {
	mu      sync.Mutex
	agents  map[string]*Gortn
	baseDir *fs.StaticDir
	fsys    *fs.FS
	user    string
}

func NewGortnManager(fsys *fs.FS, root *fs.StaticDir, user string) *GortnManager {
	gm := &GortnManager{
		agents: make(map[string]*Gortn),
		fsys:   fsys,
		user:   user,
	}
	gm.baseDir = fs.NewStaticDir(fsys.NewStat("gortns", user, user, 0755|proto.DMDIR))
	root.AddChild(gm.baseDir)
	gm.baseDir.AddChild(&gortnDirCtl{gm: gm, BaseFile: fs.NewBaseFile(fsys.NewStat("ctl", user, user, 0222))})
	log.Printf("gortns manager initialized at /gortns/")
	return gm
}

// Spawn creates a new agent directory under /gortns/<id>/
// Must hold gm.mu if called from outside the spawn path.
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

	dir.AddChild(&gortnStatusFile{g: g, BaseFile: fs.NewBaseFile(gm.fsys.NewStat("status", gm.user, gm.user, 0444))})
	dir.AddChild(&gortnCtlFile{g: g, BaseFile: fs.NewBaseFile(gm.fsys.NewStat("ctl", gm.user, gm.user, 0222))})
	dir.AddChild(&gortnInFile{g: g, BaseFile: fs.NewBaseFile(gm.fsys.NewStat("in", gm.user, gm.user, 0222))})
	dir.AddChild(&gortnOutFile{g: g, BaseFile: fs.NewBaseFile(gm.fsys.NewStat("out", gm.user, gm.user, 0444))})
	dir.AddChild(&gortnArgFile{g: g, BaseFile: fs.NewBaseFile(gm.fsys.NewStat("arg", gm.user, gm.user, 0666))})

	go g.run()
	return nil
}

// Remove stops an agent and deletes its directory entry.
func (gm *GortnManager) Remove(id string) error {
	gm.mu.Lock()
	defer gm.mu.Unlock()

	g, exists := gm.agents[id]
	if !exists {
		return fmt.Errorf("agent %s not found", id)
	}

	g.stopAgent()
	delete(gm.agents, id)
	gm.baseDir.DeleteChild(id)
	log.Printf("[gortns] removed %s", id)
	return nil
}

// --- Agent struct and lifecycle ---

type GortnState string

const (
	StateIdle    GortnState = "idle"
	StateRunning GortnState = "running"
	StateStopped GortnState = "stopped"
)

type Gortn struct {
	gm      *GortnManager
	id      string
	thought string
	state   GortnState
	status  string

	// Output ring buffer
	mu      sync.Mutex
	outBuf  bytes.Buffer
	outWake chan struct{}  // signals readers that new data arrived
	closed  bool

	// Channels
	in   chan []byte
	ctl  chan string
	stop chan struct{}

	// Per-open read tracking (fid -> byte offset consumed so far)
	readers   map[uint64]uint64
	readersMu sync.Mutex
}

func newGortn(gm *GortnManager, id, thought string) *Gortn {
	return &Gortn{
		gm:      gm,
		id:      id,
		thought: thought,
		state:   StateIdle,
		in:      make(chan []byte, 256),
		ctl:     make(chan string, 8),
		stop:    make(chan struct{}),
		outWake: make(chan struct{}, 1),
		readers: make(map[uint64]uint64),
	}
}

func (g *Gortn) setState(s GortnState, detail string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.state = s
	g.status = detail
}

func (g *Gortn) getState() (GortnState, string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.state, g.status
}

// writeOut appends data to the ring buffer and wakes readers.
func (g *Gortn) writeOut(data []byte) {
	g.mu.Lock()
	g.outBuf.Write(data)
	g.mu.Unlock()

	select {
	case g.outWake <- struct{}{}:
	default:
	}
}

// openReader registers a new fid for reading /gortns/<id>/out.
func (g *Gortn) openReader(fid uint64) {
	g.readersMu.Lock()
	g.readers[fid] = 0
	g.readersMu.Unlock()
}

// closeReader removes a fid's read tracking.
func (g *Gortn) closeReader(fid uint64) {
	g.readersMu.Lock()
	delete(g.readers, fid)
	g.readersMu.Unlock()
}

// readOut reads from the ring buffer starting at fid's offset,
// blocking until data is available or the agent is stopped.
func (g *Gortn) readOut(fid uint64, count uint64) ([]byte, error) {
	for {
		g.readersMu.Lock()
		offset := g.readers[fid]
		g.readersMu.Unlock()

		g.mu.Lock()
		bufLen := uint64(g.outBuf.Len())
		avail := bufLen - offset
		if avail > 0 {
			n := count
			if n > avail {
				n = avail
			}
			data := make([]byte, n)
			raw := g.outBuf.Bytes()
			copy(data, raw[offset:offset+n])
			g.readersMu.Lock()
			g.readers[fid] = offset + n
			g.readersMu.Unlock()
			g.mu.Unlock()
			return data, nil
		}

		if g.closed {
			g.mu.Unlock()
			return nil, io.EOF
		}
		g.mu.Unlock()

		// No data available — wait
		select {
		case <-g.outWake:
			continue
		case <-g.stop:
			return nil, io.EOF
		case <-time.After(30 * time.Second):
			continue
		}
	}
}

func (g *Gortn) run() {
	log.Printf("[gortns %s] spawned: %s", g.id, g.thought)
	g.writeOut([]byte(fmt.Sprintf("[gortns %s] %s\n", g.id, g.thought)))

	for {
		select {
		case cmd := <-g.ctl:
			g.handleCtl(strings.TrimSpace(cmd))
		case <-g.stop:
			g.mu.Lock()
			g.state = StateStopped
			g.status = "stopped"
			closed := g.closed
			if !closed {
				g.closed = true
				g.outBuf.WriteString("[gortns " + g.id + "] stopped\n")
			}
			g.mu.Unlock()
			// Wake anyone waiting for data
			select {
			case g.outWake <- struct{}{}:
			default:
			}
			return
		}
	}
}

func (g *Gortn) handleCtl(cmd string) {
	switch {
	case cmd == "stop":
		g.stopAgent()

	case strings.HasPrefix(cmd, "exec "):
		g.execCommand(strings.TrimPrefix(cmd, "exec "))

	case strings.HasPrefix(cmd, "spawn "):
		subThought := strings.TrimPrefix(cmd, "spawn ")
		id := fmt.Sprintf("%s.%d", g.id, time.Now().UnixNano())
		if err := g.gm.Spawn(id, subThought); err != nil {
			g.writeOut([]byte(fmt.Sprintf("[gortns %s] spawn error: %s\n", g.id, err)))
		} else {
			g.writeOut([]byte(fmt.Sprintf("[gortns %s] spawned %s: %s\n", g.id, id, subThought)))
		}

	case strings.HasPrefix(cmd, "echo "):
		g.writeOut([]byte(strings.TrimPrefix(cmd, "echo ") + "\n"))

	default:
		g.writeOut([]byte(fmt.Sprintf("[gortns %s] unknown cmd: %s\n", g.id, cmd)))
	}
}

func (g *Gortn) execCommand(shellCmd string) {
	g.setState(StateRunning, "exec: "+shellCmd)
	g.writeOut([]byte("$ " + shellCmd + "\n"))

	cmd := exec.Command("/bin/sh", "-c", shellCmd)
	cmd.Dir = os.Getenv("HOME")

	stdin, _ := cmd.StdinPipe()
	go func() {
		defer stdin.Close()
		for {
			select {
			case data := <-g.in:
				stdin.Write(data)
			case <-g.stop:
				return
			}
		}
	}()

	var wg sync.WaitGroup
	for _, stream := range []io.ReadCloser{stdoutPipe(cmd), stderrPipe(cmd)} {
		if stream == nil {
			continue
		}
		wg.Add(1)
		go func(r io.Reader) {
			defer wg.Done()
			buf := make([]byte, 8192)
			for {
				n, err := r.Read(buf)
				if n > 0 {
					chunk := make([]byte, n)
					copy(chunk, buf[:n])
					g.writeOut(chunk)
				}
				if err != nil {
					return
				}
			}
		}(stream)
	}

	err := cmd.Run()
	wg.Wait()

	if err == nil {
		g.setState(StateIdle, "done: 0")
	} else if exitErr, ok := err.(*exec.ExitError); ok {
		g.setState(StateIdle, fmt.Sprintf("done: %d", exitErr.ExitCode()))
	} else {
		g.setState(StateIdle, fmt.Sprintf("error: %s", err))
	}
}

func (g *Gortn) stopAgent() {
	select {
	case g.stop <- struct{}{}:
	default:
	}
}

func stdoutPipe(cmd *exec.Cmd) io.ReadCloser {
	r, _ := cmd.StdoutPipe()
	return r
}

func stderrPipe(cmd *exec.Cmd) io.ReadCloser {
	r, _ := cmd.StderrPipe()
	return r
}

// --- 9P files ---

// /gortns/ctl - spawn agents
type gortnDirCtl struct {
	*fs.BaseFile
	gm *GortnManager
}

func (f *gortnDirCtl) Write(fid uint64, offset uint64, data []byte) (uint32, error) {
	cmd := strings.TrimSpace(string(data))
	if strings.HasPrefix(cmd, "spawn ") {
		thought := strings.TrimPrefix(cmd, "spawn ")
		id := fmt.Sprintf("t%d", time.Now().UnixNano())
		if err := f.gm.Spawn(id, thought); err != nil {
			return 0, err
		}
	}
	return uint32(len(data)), nil
}

// /gortns/<id>/status
type gortnStatusFile struct {
	*fs.BaseFile
	g *Gortn
}

func (f *gortnStatusFile) Read(fid uint64, offset uint64, count uint64) ([]byte, error) {
	state, detail := f.g.getState()
	line := string(state)
	if detail != "" {
		line += " " + detail
	}
	line += "\n"
	data := []byte(line)
	if offset >= uint64(len(data)) {
		return nil, io.EOF
	}
	end := offset + count
	if end > uint64(len(data)) {
		end = uint64(len(data))
	}
	return data[offset:end], nil
}

// /gortns/<id>/ctl
type gortnCtlFile struct {
	*fs.BaseFile
	g *Gortn
}

func (f *gortnCtlFile) Write(fid uint64, offset uint64, data []byte) (uint32, error) {
	f.g.ctl <- strings.TrimSpace(string(data))
	return uint32(len(data)), nil
}

// /gortns/<id>/in
type gortnInFile struct {
	*fs.BaseFile
	g *Gortn
}

func (f *gortnInFile) Write(fid uint64, offset uint64, data []byte) (uint32, error) {
	select {
	case f.g.in <- data:
	default:
	}
	return uint32(len(data)), nil
}

// /gortns/<id>/out - buffered, blocking, offset-aware output stream
type gortnOutFile struct {
	*fs.BaseFile
	g *Gortn
}

func (f *gortnOutFile) Open(fid uint64, mode proto.Mode) error {
	f.g.openReader(fid)
	return nil
}

func (f *gortnOutFile) Read(fid uint64, offset uint64, count uint64) ([]byte, error) {
	return f.g.readOut(fid, count)
}

func (f *gortnOutFile) Clunk(fid uint64) error {
	f.g.closeReader(fid)
	return nil
}

// /gortns/<id>/arg - task argument passed from external caller
// Write sets it, Read gets it, clobbered on next write.
type gortnArgFile struct {
	*fs.BaseFile
	g *Gortn
}

func (f *gortnArgFile) Read(fid uint64, offset uint64, count uint64) ([]byte, error) {
	f.g.mu.Lock()
	defer f.g.mu.Unlock()
	// re-read of arg not supported well; return at offset
	return nil, io.EOF
}

func (f *gortnArgFile) Write(fid uint64, offset uint64, data []byte) (uint32, error) {
	f.g.mu.Lock()
	defer f.g.mu.Unlock()
	// The caller sets the task arg. It lands in the control channel
	// prefixed with "arg:" so the agent can act on it.
	arg := strings.TrimSpace(string(data))
	if arg != "" {
		// Push it into ctl channel as an arg command
		select {
		case f.g.ctl <- "exec " + arg:
		default:
		}
	}
	return uint32(len(data)), nil
}
