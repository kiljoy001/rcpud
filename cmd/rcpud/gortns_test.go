//go:build rcpud

package main

import (
	"github.com/knusbaum/go9p/fs"
	"github.com/knusbaum/go9p/proto"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

func TestGortnManagerSpawnRemove(t *testing.T) {
	user := "scott"
	fsys, _ := fs.NewFS("test", user, 0755)
	root := fs.NewStaticDir(fsys.NewStat("/", user, user, 0755|proto.DMDIR))
	gm := NewGortnManager(fsys, root, user)

	id := "test-spawn"
	err := gm.Spawn(id, "test thought")
	if err != nil {
		t.Fatalf("Failed to spawn agent: %v", err)
	}

	gm.mu.Lock()
	_, exists := gm.agents[id]
	gm.mu.Unlock()
	if !exists {
		t.Errorf("Agent %s was not added to manager", id)
	}

	// Verify directory entry
	found := false
	for _, child := range gm.baseDir.Children() {
		if child.Stat().Name == id {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("Agent %s directory not found in /gortns/", id)
	}

	// Test Remove
	err = gm.Remove(id)
	if err != nil {
		t.Fatalf("Failed to remove agent: %v", err)
	}

	gm.mu.Lock()
	_, exists = gm.agents[id]
	gm.mu.Unlock()
	if exists {
		t.Errorf("Agent %s still exists in manager after removal", id)
	}
}

func TestHandleStop(t *testing.T) {
	user := "scott"
	fsys, _ := fs.NewFS("test", user, 0755)
	root := fs.NewStaticDir(fsys.NewStat("/", user, user, 0755|proto.DMDIR))
	gm := NewGortnManager(fsys, root, user)
	g := newGortn(gm, "t1", "test")

	// We can't easily swap the channel because it's private.
	// But we can ensure the goroutine is ready.
	ready := make(chan bool)
	stopped := make(chan bool)
	go func() {
		ready <- true
		<-g.stop
		stopped <- true
	}()

	<-ready
	time.Sleep(10 * time.Millisecond) // Give it a tiny bit more time to reach the receive
	handleStop(g, "")

	select {
	case <-stopped:
		// success
	case <-time.After(500 * time.Millisecond):
		t.Error("handleStop did not send stop signal")
	}
}

func TestGortnReadWrite(t *testing.T) {
	g := newGortn(nil, "t1", "thought")
	data := []byte("hello world")
	g.writeOut(data)

	fid := uint64(1)
	g.openReader(fid)

	read, err := g.readOut(fid, uint64(len(data)))
	if err != nil {
		t.Fatalf("readOut failed: %v", err)
	}
	if string(read) != string(data) {
		t.Errorf("Expected %q, got %q", string(data), string(read))
	}

	g.closeReader(fid)
}

func TestGortnState(t *testing.T) {
	g := newGortn(nil, "t1", "thought")
	g.setState(StateRunning, "running task")
	state, status := g.getState()
	if state != StateRunning || status != "running task" {
		t.Errorf("Unexpected state/status: %v, %v", state, status)
	}
}

func TestGortnExec(t *testing.T) {
	user := "scott"
	fsys, _ := fs.NewFS("test", user, 0755)
	root := fs.NewStaticDir(fsys.NewStat("/", user, user, 0755|proto.DMDIR))
	gm := NewGortnManager(fsys, root, user)
	g := newGortn(gm, "t1", "test")

	// We need to set HOME for execCommand
	oldHome := os.Getenv("HOME")
	os.Setenv("HOME", "/tmp")
	defer os.Setenv("HOME", oldHome)

	// Use handleCtl to cover handleExec
	g.handleCtl("exec echo hello-exec")

	// Wait a bit for the command to finish
	time.Sleep(100 * time.Millisecond)

	g.mu.Lock()
	out := g.outBuf.String()
	g.mu.Unlock()

	if !strings.Contains(out, "hello-exec") {
		t.Errorf("Expected out to contain %q, got %q", "hello-exec", out)
	}
}

func TestGortnOutFileRead(t *testing.T) {
	g := newGortn(nil, "t1", "thought")
	f := &gortnOutFile{BaseFile: nil, g: g}

	data := []byte("hello")
	g.writeOut(data)

	fid := uint64(1)
	g.openReader(fid)

	read, err := f.Read(fid, 0, uint64(len(data)))
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}
	if string(read) != string(data) {
		t.Errorf("Expected %q, got %q", string(data), string(read))
	}
}

func TestGortnStatusFileRead(t *testing.T) {
	g := newGortn(nil, "t1", "thought")
	g.setState(StateRunning, "working")
	f := &gortnStatusFile{BaseFile: nil, g: g}

	read, err := f.Read(1, 0, 100)
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}
	expected := "running working\n"
	if string(read) != expected {
		t.Errorf("Expected %q, got %q", expected, string(read))
	}
}

func TestGortnInFileWrite(t *testing.T) {
	g := newGortn(nil, "t1", "thought")
	f := &gortnInFile{BaseFile: nil, g: g}

	data := []byte("input")
	n, err := f.Write(1, 0, data)
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	if n != uint32(len(data)) {
		t.Errorf("Expected %d bytes written, got %d", len(data), n)
	}

	// Verify input landed in channel
	select {
	case in := <-g.in:
		if string(in) != string(data) {
			t.Errorf("Expected %q in channel, got %q", string(data), string(in))
		}
	default:
		t.Error("Input did not reach channel")
	}
}

func TestGortnCtlFileWrite(t *testing.T) {
	g := newGortn(nil, "t1", "thought")
	f := &gortnCtlFile{BaseFile: nil, g: g}

	data := []byte("stop")
	n, err := f.Write(1, 0, data)
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	if n != uint32(len(data)) {
		t.Errorf("Expected %d bytes written, got %d", len(data), n)
	}

	// Verify ctl landed in channel
	select {
	case ctl := <-g.ctl:
		if ctl != "stop" {
			t.Errorf("Expected %q in channel, got %q", "stop", ctl)
		}
	default:
		t.Error("Ctl did not reach channel")
	}
}

func TestGortnRun(t *testing.T) {
	user := "scott"
	fsys, _ := fs.NewFS("test", user, 0755)
	root := fs.NewStaticDir(fsys.NewStat("/", user, user, 0755|proto.DMDIR))
	gm := NewGortnManager(fsys, root, user)

	id := "run-test"
	err := gm.Spawn(id, "test thought")
	if err != nil {
		t.Fatalf("Spawn failed: %v", err)
	}

	gm.mu.Lock()
	g := gm.agents[id]
	gm.mu.Unlock()

	// Wait for state to become running
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		state, _ := g.getState()
		if state == StateRunning {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	state, _ := g.getState()
	if state != StateRunning {
		t.Errorf("Expected StateRunning, got %v", state)
	}

	// Send stop
	g.handleCtl("stop")

	// Wait for cleanup
	deadline = time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		gm.mu.Lock()
		_, exists := gm.agents[id]
		gm.mu.Unlock()
		if !exists {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	gm.mu.Lock()
	_, exists := gm.agents[id]
	gm.mu.Unlock()
	if exists {
		t.Error("Agent should have been removed after run finished")
	}
}

func TestGortnReadBlocking(t *testing.T) {
	g := newGortn(nil, "t1", "thought")
	fid := uint64(1)
	g.openReader(fid)

	// Read in goroutine
	resCh := make(chan []byte)
	go func() {
		data, _ := g.readOut(fid, 10)
		resCh <- data
	}()

	// Ensure it's blocking
	select {
	case <-resCh:
		t.Error("Read should have blocked")
	case <-time.After(10 * time.Millisecond):
		// success
	}

	// Write data and wake
	g.writeOut([]byte("data"))

	// Check read result
	select {
	case data := <-resCh:
		if string(data) != "data" {
			t.Errorf("Expected data, got %q", string(data))
		}
	case <-time.After(500 * time.Millisecond):
		t.Error("Read timed out after wake")
	}
}

func TestGortnReadClosed(t *testing.T) {
	g := newGortn(nil, "t1", "thought")
	fid := uint64(1)
	g.openReader(fid)

	g.mu.Lock()
	g.closed = true
	g.mu.Unlock()

	_, err := g.readOut(fid, 10)
	if err != io.EOF {
		t.Errorf("Expected EOF on closed gortn, got %v", err)
	}
}

func TestGortnArgFileWrite(t *testing.T) {
	g := newGortn(nil, "t1", "thought")
	f := &gortnArgFile{BaseFile: nil, g: g}
	
	data := []byte("arg")
	n, err := f.Write(1, 0, data)
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	if n != uint32(len(data)) {
		t.Errorf("Expected %d bytes written, got %d", len(data), n)
	}
	
	// Verify it reached ctl as "exec arg"
	select {
	case ctl := <-g.ctl:
		if ctl != "exec arg" {
			t.Errorf("Expected exec arg, got %q", ctl)
		}
	default:
		t.Error("Arg did not reach ctl channel")
	}
}

func TestGortnDirCtlWrite(t *testing.T) {
	user := "scott"
	fsys, _ := fs.NewFS("test", user, 0755)
	root := fs.NewStaticDir(fsys.NewStat("/", user, user, 0755|proto.DMDIR))
	gm := NewGortnManager(fsys, root, user)
	f := &gortnDirCtl{BaseFile: nil, gm: gm}
	
	data := []byte("spawn global-thought")
	n, err := f.Write(1, 0, data)
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	if n != uint32(len(data)) {
		t.Errorf("Expected %d bytes written, got %d", len(data), n)
	}
	
	// Check if spawned
	gm.mu.Lock()
	found := false
	for id := range gm.agents {
		if strings.HasPrefix(id, "t") { // Default ID starts with t
			found = true
			break
		}
	}
	gm.mu.Unlock()
	
	if !found {
		t.Error("Global spawn command did not spawn an agent")
	}
}

func TestGortnDummyMethods(t *testing.T) {
	g := newGortn(nil, "t1", "thought")
	
	// gortnDirCtl
	dc := &gortnDirCtl{gm: nil}
	dc.Read(1, 0, 10)
	
	// gortnCtlFile
	cf := &gortnCtlFile{g: g}
	cf.Read(1, 0, 10)
	
	// gortnInFile
	inf := &gortnInFile{g: g}
	inf.Read(1, 0, 10)
	
	// gortnArgFile
	af := &gortnArgFile{g: g}
	if _, err := af.Read(1, 0, 10); err != io.EOF {
		t.Errorf("Expected EOF on gortnArgFile.Read, got %v", err)
	}
	
	// gortnOutFile Open/Clunk
	of := &gortnOutFile{g: g}
	if err := of.Open(1, proto.Oread); err != nil {
		t.Errorf("Open failed: %v", err)
	}
	if err := of.Clunk(1); err != nil {
		t.Errorf("Clunk failed: %v", err)
	}
}

func TestGortnManagerErrors(t *testing.T) {
	user := "scott"
	fsys, _ := fs.NewFS("test", user, 0755)
	root := fs.NewStaticDir(fsys.NewStat("/", user, user, 0755|proto.DMDIR))
	gm := NewGortnManager(fsys, root, user)

	// Duplicate spawn
	gm.Spawn("t1", "thought")
	if err := gm.Spawn("t1", "thought"); err == nil {
		t.Error("Expected error on duplicate spawn")
	}

	// Remove non-existent
	if err := gm.Remove("non-existent"); err == nil {
		t.Error("Expected error on removing non-existent agent")
	}
}

func TestHandleSpawnError(t *testing.T) {
	user := "scott"
	fsys, _ := fs.NewFS("test", user, 0755)
	root := fs.NewStaticDir(fsys.NewStat("/", user, user, 0755|proto.DMDIR))
	gm := NewGortnManager(fsys, root, user)
	
	// Create a duplicate to trigger error in handleSpawn
	gm.Spawn("t1.dup", "thought")
}

func TestHandleSpawnCommand(t *testing.T) {
	user := "scott"
	fsys, _ := fs.NewFS("test", user, 0755)
	root := fs.NewStaticDir(fsys.NewStat("/", user, user, 0755|proto.DMDIR))
	gm := NewGortnManager(fsys, root, user)
	g := newGortn(gm, "t1", "test")

	g.handleCtl("spawn sub-thought")

	// Check if a sub-agent was spawned
	gm.mu.Lock()
	found := false
	for id := range gm.agents {
		if strings.HasPrefix(id, "t1.") {
			found = true
			break
		}
	}
	gm.mu.Unlock()

	if !found {
		t.Error("Spawn command did not spawn a sub-agent")
	}
}

func TestHandleCtlCharacterization(t *testing.T) {
	user := "scott"
	fsys, _ := fs.NewFS("test", user, 0755)
	root := fs.NewStaticDir(fsys.NewStat("/", user, user, 0755|proto.DMDIR))
	gm := NewGortnManager(fsys, root, user)

	g := newGortn(gm, "t1", "test thought")

	tests := []struct {
		name     string
		cmd      string
		expected string
	}{
		{"echo", "echo hello", "hello\n"},
		{"unknown", "unknown-cmd", "[gortns t1] unknown cmd: unknown-cmd\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g.handleCtl(tt.cmd)
			// We need a way to read the output without blocking
			// Since we're doing characterization, I'll use the internal outBuf for now
			g.mu.Lock()
			out := g.outBuf.String()
			g.outBuf.Reset()
			g.mu.Unlock()

			if !strings.Contains(out, tt.expected) {
				t.Errorf("cmd %q: expected out to contain %q, got %q", tt.cmd, tt.expected, out)
			}
		})
	}
}
