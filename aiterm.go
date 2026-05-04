//go:build aiterm

package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/knusbaum/go9p"
	"github.com/knusbaum/go9p/fs"
	"github.com/knusbaum/go9p/proto"
)

/*
 * aiterm.go - The 9P-Native AI Agent Shell
 * Standalone binary that serves /gortns/<id>/{status,ctl,in,out}
 * over TCP on 127.0.0.1:5640. Mount via 9pfuse for namespace access.
 */

// agentBuf is a ring-buffer output stream with per-fid offset tracking.
type agentBuf struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	closed  bool
	wake    chan struct{}
	readers map[uint64]uint64
}

func newAgentBuf() *agentBuf {
	return &agentBuf{
		wake:    make(chan struct{}, 1),
		readers: make(map[uint64]uint64),
	}
}

func (ab *agentBuf) Write(data []byte) {
	ab.mu.Lock()
	ab.buf.Write(data)
	ab.mu.Unlock()
	select {
	case ab.wake <- struct{}{}:
	default:
	}
}

func (ab *agentBuf) Close() {
	ab.mu.Lock()
	ab.closed = true
	ab.mu.Unlock()
	select {
	case ab.wake <- struct{}{}:
	default:
	}
}

func (ab *agentBuf) OpenRead(fid uint64) {
	ab.mu.Lock()
	ab.readers[fid] = uint64(ab.buf.Len())
	ab.mu.Unlock()
}

func (ab *agentBuf) CloseRead(fid uint64) {
	ab.mu.Lock()
	delete(ab.readers, fid)
	ab.mu.Unlock()
}

func (ab *agentBuf) Read(fid uint64, count uint64) ([]byte, error) {
	for {
		ab.mu.Lock()
		offset := ab.readers[fid]
		total := uint64(ab.buf.Len())
		avail := total - offset

		if avail > 0 {
			n := count
			if n > avail {
				n = avail
			}
			data := make([]byte, n)
			raw := ab.buf.Bytes()
			copy(data, raw[offset:offset+n])
			ab.readers[fid] = offset + n
			ab.mu.Unlock()
			return data, nil
		}

		if ab.closed {
			ab.mu.Unlock()
			return nil, io.EOF
		}
		ab.mu.Unlock()

		select {
		case <-ab.wake:
			continue
		case <-time.After(30 * time.Second):
			continue
		}
	}
}

// Agent is a goroutine-backed actor with a 9P filesystem presence.
type Agent struct {
	id     string
	out    *agentBuf
	in     chan []byte
	ctl    chan string
	stop   chan struct{}
	state  string // "running" or "stopped"
	status string // detailed status line (e.g. "done: 0")
	mu     sync.Mutex
}

func newAgent(id string) *Agent {
	return &Agent{
		id:   id,
		out:  newAgentBuf(),
		in:   make(chan []byte, 256),
		ctl:  make(chan string, 8),
		stop: make(chan struct{}),
		state: "running",
	}
}

func (a *Agent) setState(state, detail string) {
	a.mu.Lock()
	a.state = state
	a.status = detail
	a.mu.Unlock()
}

func (a *Agent) run(thought string) {
	a.out.Write([]byte(fmt.Sprintf("[gortns %s] %s\n", a.id, thought)))
	for {
		select {
		case cmd := <-a.ctl:
			cmd = strings.TrimSpace(cmd)
			switch {
			case cmd == "stop":
				a.setState("stopped", "")
				a.out.Write([]byte(fmt.Sprintf("[gortns %s] stopped\n", a.id)))
				a.out.Close()
				return
			case strings.HasPrefix(cmd, "exec "):
				a.execCommand(strings.TrimPrefix(cmd, "exec "))
			case strings.HasPrefix(cmd, "echo "):
				a.out.Write([]byte(strings.TrimPrefix(cmd, "echo ") + "\n"))
			default:
				a.out.Write([]byte(fmt.Sprintf("[gortns %s] unknown: %s\n", a.id, cmd)))
			}
		case <-a.stop:
			a.setState("stopped", "")
			a.out.Write([]byte(fmt.Sprintf("[gortns %s] stopped\n", a.id)))
			a.out.Close()
			return
		}
	}
}

func (a *Agent) execCommand(shellCmd string) {
	a.setState("running", "exec: "+shellCmd)
	a.out.Write([]byte("$ " + shellCmd + "\n"))

	cmd := exec.Command("/bin/sh", "-c", shellCmd)
	cmd.Dir = os.Getenv("HOME")

	stdin, _ := cmd.StdinPipe()
	go func() {
		defer stdin.Close()
		for {
			select {
			case data := <-a.in:
				stdin.Write(data)
			case <-a.stop:
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
					a.out.Write(chunk)
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
		a.setState("idle", "done: 0")
	} else if ex, ok := err.(*exec.ExitError); ok {
		a.setState("idle", fmt.Sprintf("done: %d", ex.ExitCode()))
	} else {
		a.setState("idle", fmt.Sprintf("error: %s", err))
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

// --- 9P file implementations ---

// AgentStatusFile
type AgentStatusFile struct {
	*fs.BaseFile
	agent *Agent
}

func (f *AgentStatusFile) Read(fid uint64, offset uint64, count uint64) ([]byte, error) {
	f.agent.mu.Lock()
	state := f.agent.state
	status := f.agent.status
	f.agent.mu.Unlock()

	line := state
	if status != "" {
		line += " " + status
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

// AgentCtlFile
type AgentCtlFile struct {
	*fs.BaseFile
	agent *Agent
}

func (f *AgentCtlFile) Write(fid uint64, offset uint64, data []byte) (uint32, error) {
	f.agent.ctl <- strings.TrimSpace(string(data))
	return uint32(len(data)), nil
}

// AgentInFile
type AgentInFile struct {
	*fs.BaseFile
	agent *Agent
}

func (f *AgentInFile) Write(fid uint64, offset uint64, data []byte) (uint32, error) {
	select {
	case f.agent.in <- data:
	default:
	}
	return uint32(len(data)), nil
}

// AgentOutFile — ring-buffered, blocking, offset-aware
type AgentOutFile struct {
	*fs.BaseFile
	agent *Agent
}

func (f *AgentOutFile) Open(fid uint64, mode proto.Mode) error {
	f.agent.out.OpenRead(fid)
	return nil
}

func (f *AgentOutFile) Read(fid uint64, offset uint64, count uint64) ([]byte, error) {
	return f.agent.out.Read(fid, count)
}

func (f *AgentOutFile) Clunk(fid uint64) error {
	f.agent.out.CloseRead(fid)
	return nil
}

// --- main ---

func main() {
	fmt.Println("AI TERM (9P-Native Agent Shell) Online.")

	user := os.Getenv("USER")
	if user == "" {
		user = "scott"
	}

	fsys, groot := fs.NewFS("aiterm", user, 0755)
	gortnsDir := fs.NewStaticDir(fsys.NewStat("gortns", user, user, 0755|proto.DMDIR))
	groot.AddChild(gortnsDir)

	// Serve 9P filesystem over TCP
	go func() {
		ln, err := net.Listen("tcp", "127.0.0.1:5640")
		if err != nil {
			log.Fatalf("listen: %v", err)
		}
		fmt.Println("[9P] Listening on 127.0.0.1:5640")
		for {
			conn, err := ln.Accept()
			if err != nil {
				continue
			}
			go go9p.ServeReadWriter(conn, conn, fsys.Server())
		}
	}()

	// Main REPL loop
	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Print("aiterm% ")
		input, err := reader.ReadString('\n')
		if err != nil {
			break
		}
		cmdStr := strings.TrimSpace(input)
		if cmdStr == "" {
			continue
		}
		if cmdStr == "exit" || cmdStr == "quit" {
			break
		}
		handleInput(cmdStr, user, fsys, gortnsDir)
	}
}

func handleInput(cmdStr string, user string, fsys *fs.FS, gortnsDir *fs.StaticDir) bool {
	if strings.HasPrefix(cmdStr, "routine ") {
		spawnGortn(fsys, gortnsDir, cmdStr[8:], user)
		return true
	}
	// Fallback to rc
	executeInRc(cmdStr)
	return true
}

func spawnGortn(fsys *fs.FS, dir *fs.StaticDir, thought, user string) {
	id := fmt.Sprintf("t%d", time.Now().UnixNano())
	fmt.Printf("[Gortn Spawned]: %s (ID: %s)\n", thought, id)

	agent := newAgent(id)
	agentDir := fs.NewStaticDir(fsys.NewStat(id, user, user, 0755|proto.DMDIR))
	dir.AddChild(agentDir)

	agentDir.AddChild(&AgentStatusFile{
		BaseFile: fs.NewBaseFile(fsys.NewStat("status", user, user, 0444)),
		agent:    agent,
	})
	agentDir.AddChild(&AgentCtlFile{
		BaseFile: fs.NewBaseFile(fsys.NewStat("ctl", user, user, 0222)),
		agent:    agent,
	})
	agentDir.AddChild(&AgentInFile{
		BaseFile: fs.NewBaseFile(fsys.NewStat("in", user, user, 0222)),
		agent:    agent,
	})
	agentDir.AddChild(&AgentOutFile{
		BaseFile: fs.NewBaseFile(fsys.NewStat("out", user, user, 0444)),
		agent:    agent,
	})

	go agent.run(thought)
}

func executeInRc(cmdStr string) {
	shell := "/usr/local/plan9/bin/rc"
	if _, err := os.Stat(shell); err != nil {
		shell = "/bin/sh"
	}
	c := exec.Command(shell, "-c", cmdStr)
	c.Stdin = os.Stdin
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	if err := c.Run(); err != nil {
		fmt.Printf("[Shell Error]: %v\n", err)
	}
}
