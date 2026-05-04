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

// NewGortnManager creates a /gortns/ directory on the given filesystem.
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
	dir.AddChild(&gortnMemFile{g: g, BaseFile: fs.NewBaseFile(gm.fsys.NewStat("mem", gm.user, gm.user, 0666))})

	go g.run()
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
	status  string // detailed status line for "exec: go test ./..." or "done: 0"
	in      chan []byte
	out     chan []byte
	memBuf  bytes.Buffer // persistent memory
	ctl     chan string
	stop    chan struct{}
	mu      sync.Mutex
}

func newGortn(gm *GortnManager, id, thought string) *Gortn {
	return &Gortn{
		gm:      gm,
		id:      id,
		thought: thought,
		state:   StateIdle,
		in:      make(chan []byte, 256),
		out:     make(chan []byte, 256),
		ctl:     make(chan string, 8),
		stop:    make(chan struct{}),
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

func (g *Gortn) run() {
	log.Printf("[gortns %s] spawned: %s", g.id, g.thought)
	g.out <- []byte(fmt.Sprintf("[gortns %s] %s\n", g.id, g.thought))

	for {
		select {
		case cmd := <-g.ctl:
			cmd = strings.TrimSpace(cmd)
			g.handleCtl(cmd)

		case <-g.stop:
			g.setState(StateStopped, "stopped")
			g.out <- []byte(fmt.Sprintf("[gortns %s] stopped\n", g.id))
			return
		}
	}
}

func (g *Gortn) handleCtl(cmd string) {
	switch {
	case cmd == "stop":
		g.setState(StateStopped, "stopped")
		g.out <- []byte(fmt.Sprintf("[gortns %s] stopped\n", g.id))
		return

	case strings.HasPrefix(cmd, "exec "):
		shellCmd := strings.TrimPrefix(cmd, "exec ")
		g.execCommand(shellCmd)

	case strings.HasPrefix(cmd, "spawn "):
		subThought := strings.TrimPrefix(cmd, "spawn ")
		id := fmt.Sprintf("%s.%d", g.id, time.Now().UnixNano())
		if err := g.gm.Spawn(id, subThought); err != nil {
			g.out <- []byte(fmt.Sprintf("[gortns %s] spawn error: %s\n", g.id, err))
		} else {
			g.out <- []byte(fmt.Sprintf("[gortns %s] spawned %s: %s\n", g.id, id, subThought))
		}

	case strings.HasPrefix(cmd, "echo "):
		msg := strings.TrimPrefix(cmd, "echo ")
		g.out <- []byte(msg + "\n")

	default:
		g.out <- []byte(fmt.Sprintf("[gortns %s] unknown cmd: %s\n", g.id, cmd))
	}
}

// execCommand runs a shell command with stdin from g.in, stdout to g.out.
func (g *Gortn) execCommand(shellCmd string) {
	g.setState(StateRunning, "exec: "+shellCmd)
	g.out <- []byte(fmt.Sprintf("$ %s\n", shellCmd))

	cmd := exec.Command("/bin/sh", "-c", shellCmd)
	cmd.Dir = os.Getenv("HOME")

	// Stdin from the agent's input channel
	stdin, _ := cmd.StdinPipe()
	go func() {
		defer stdin.Close()
		for {
			select {
			case data := <-g.in:
				stdin.Write(data)
			case <-time.After(100 * time.Millisecond):
				// Non-blocking check for stop
				select {
				case <-g.stop:
					return
				default:
				}
			}
		}
	}()

	// Stdout + stderr interleaved into out channel
	var wg sync.WaitGroup
	for _, stream := range []io.ReadCloser{stdoutPipe(cmd), stderrPipe(cmd)} {
		if stream == nil {
			continue
		}
		wg.Add(1)
		go func(r io.Reader) {
			defer wg.Done()
			buf := make([]byte, 4096)
			for {
				n, err := r.Read(buf)
				if n > 0 {
					chunk := make([]byte, n)
					copy(chunk, buf[:n])
					g.out <- chunk
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
	} else {
		if exitErr, ok := err.(*exec.ExitError); ok {
			g.setState(StateIdle, fmt.Sprintf("done: %d", exitErr.ExitCode()))
		} else {
			g.setState(StateIdle, fmt.Sprintf("error: %s", err))
		}
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

// /gortns/<id>/in - stdin for exec'd commands
type gortnInFile struct {
	*fs.BaseFile
	g *Gortn
}

func (f *gortnInFile) Write(fid uint64, offset uint64, data []byte) (uint32, error) {
	select {
	case f.g.in <- data:
	default:
		// buffer full, drop
	}
	return uint32(len(data)), nil
}

// /gortns/<id>/out - stdout from exec'd commands
type gortnOutFile struct {
	*fs.BaseFile
	g *Gortn
}

func (f *gortnOutFile) Read(fid uint64, offset uint64, count uint64) ([]byte, error) {
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

// /gortns/<id>/mem - persistent memory for agent context
type gortnMemFile struct {
	*fs.BaseFile
	g *Gortn
}

func (f *gortnMemFile) Read(fid uint64, offset uint64, count uint64) ([]byte, error) {
	f.g.mu.Lock()
	data := f.g.memBuf.Bytes()
	f.g.mu.Unlock()

	if offset >= uint64(len(data)) {
		return nil, io.EOF
	}
	end := offset + count
	if end > uint64(len(data)) {
		end = uint64(len(data))
	}
	return data[offset:end], nil
}

func (f *gortnMemFile) Write(fid uint64, offset uint64, data []byte) (uint32, error) {
	f.g.mu.Lock()
	defer f.g.mu.Unlock()
	if offset == 0 {
		f.g.memBuf.Reset()
	}
	f.g.memBuf.Write(data)
	return uint32(len(data)), nil
}
