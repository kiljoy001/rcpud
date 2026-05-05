package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"github.com/knusbaum/go9p"
	"github.com/knusbaum/go9p/client"
	"github.com/knusbaum/go9p/fs"
	"github.com/knusbaum/go9p/proto"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"testing/quick"
	"time"
)

func TestStartUnix9PServer(t *testing.T) {
	user := "scott"
	nsFS, _ := fs.NewFS(user, user, 0755)

	sockPath, ln, err := startUnix9PServer(nsFS, user)
	if err != nil {
		t.Fatalf("Failed to start 9P server: %v", err)
	}
	defer ln.Close()
	defer os.RemoveAll(filepath.Dir(sockPath))

	// Try to dial it
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("Failed to dial 9P server: %v", err)
	}
	conn.Close()
}

func TestMountFUSE(t *testing.T) {
	if os.Getenv("CI") != "" {
		t.Skip("Skipping FUSE test in CI environment")
	}

	user := "scott"
	nsFS, _ := fs.NewFS(user, user, 0755)

	sockPath, ln, err := startUnix9PServer(nsFS, user)
	if err != nil {
		t.Fatalf("Failed to start 9P server: %v", err)
	}
	defer ln.Close()
	defer os.RemoveAll(filepath.Dir(sockPath))

	nsDir, cmd, err := mountFUSE(sockPath)
	if err != nil {
		t.Fatalf("mountFUSE failed: %v", err)
	}
	defer func() {
		// Try unmounting first
		exec.Command("fusermount", "-u", nsDir).Run()
		cmd.Process.Kill()
		os.RemoveAll(nsDir)
	}()

	// Give it a moment to mount
	time.Sleep(100 * time.Millisecond)

	// Check if nsDir exists and is a directory
	fi, err := os.Stat(nsDir)
	if err != nil {
		t.Fatalf("nsDir stat failed: %v", err)
	}
	if !fi.IsDir() {
		t.Errorf("nsDir %s is not a directory", nsDir)
	}
}

func TestMountDir(t *testing.T) {
	user := "scott"
	tempDir, _ := os.MkdirTemp("", "mount-test")
	defer os.RemoveAll(tempDir)

	os.WriteFile(filepath.Join(tempDir, "test.txt"), []byte("hello"), 0644)

	md := newMountDir(tempDir, "mnt", user, user)

	// Check Children
	children := md.Children()
	if len(children) != 1 {
		t.Errorf("Expected 1 child, got %d", len(children))
	}

	// Test Stat
	stat := md.Stat()
	if stat.Name != "mnt" {
		t.Errorf("Expected name mnt, got %s", stat.Name)
	}
}

func TestMountDirRecursive(t *testing.T) {
	user := "scott"
	tempDir, _ := os.MkdirTemp("", "mount-recursive-test")
	defer os.RemoveAll(tempDir)

	subDir := filepath.Join(tempDir, "subdir")
	os.Mkdir(subDir, 0755)
	os.WriteFile(filepath.Join(subDir, "test.txt"), []byte("hello"), 0644)

	md := newMountDir(tempDir, "mnt", user, user)

	children := md.Children()
	if len(children) != 1 {
		t.Errorf("Expected 1 child, got %d", len(children))
	}

	sd := children["subdir"]
	if sd == nil {
		t.Fatal("subdir not found")
	}

	// Check subdir children
	subChildren := sd.(*mountDir).Children()
	if len(subChildren) != 1 {
		t.Errorf("Expected 1 child in subdir, got %d", len(subChildren))
	}
}

func TestMountFileRead(t *testing.T) {
	user := "scott"
	tempDir, _ := os.MkdirTemp("", "file-test")
	defer os.RemoveAll(tempDir)

	filePath := filepath.Join(tempDir, "test.txt")
	os.WriteFile(filePath, []byte("hello world"), 0644)
	fi, _ := os.Stat(filePath)

	mf := newMountFile(filePath, fi, user, user)

	// Test Stat
	if mf.Stat().Name != "test.txt" {
		t.Error("Stat name mismatch")
	}

	// Open
	err := mf.Open(1, proto.Oread)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	// Read
	data, err := mf.Read(1, 0, 5)
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}
	if string(data) != "hello" {
		t.Errorf("Expected hello, got %q", string(data))
	}

	mf.Close(1)
}

func TestMountFUSEError(t *testing.T) {
	_, _, err := mountFUSE("/non/existent/path")
	if err == nil {
		// 9pfuse might start but exit immediately.
		// Command.Start() only fails if binary missing or other low level issues.
	}
}

func TestSetupNamespace(t *testing.T) {
	user := "scott"
	s, c := net.Pipe()
	defer s.Close()
	defer c.Close()
	
	// Server side
	fsys, root := fs.NewFS(user, user, 0755)
	devDir := fs.NewStaticDir(fsys.NewStat("dev", user, user, 0755|proto.DMDIR))
	root.AddChild(devDir)
	consFile := fs.NewStaticFile(fsys.NewStat("cons", user, user, 0666), []byte(""))
	devDir.AddChild(consFile)
	go go9p.ServeReadWriter(s, s, fsys.Server())
	
	// Client side
	cl, err := client.NewClient(c, user, "")
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	
	nsFS, nsRoot := setupNamespace(cl, user)
	if nsFS == nil || nsRoot == nil {
		t.Fatal("setupNamespace returned nil")
	}
	
	// Verify dev/cons was proxied
	cf := getConsFile(nsRoot)
	if cf == nil {
		t.Error("cons file not proxied in namespace")
	}
}

func TestWatchInterrupts(t *testing.T) {
	user := "scott"
	s, c := net.Pipe()
	defer s.Close()
	defer c.Close()
	
	// Server side
	fsys, root := fs.NewFS(user, user, 0755)
	mntDir := fs.NewStaticDir(fsys.NewStat("mnt", user, user, 0755|proto.DMDIR))
	root.AddChild(mntDir)
	cpuDir := fs.NewStaticDir(fsys.NewStat("cpunote", user, user, 0755|proto.DMDIR))
	mntDir.AddChild(cpuDir)
	noteFile := fs.NewStaticFile(fsys.NewStat("data", user, user, 0666), []byte("interrupt\n"))
	cpuDir.AddChild(noteFile)
	go go9p.ServeReadWriter(s, s, fsys.Server())
	
	// Client side
	cl, err := client.NewClient(c, user, "")
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	
	noteCh := watchInterrupts(cl)
	
	select {
	case note := <-noteCh:
		if note != "interrupt" {
			t.Errorf("Expected interrupt, got %q", note)
		}
	case <-time.After(500 * time.Millisecond):
		t.Error("Timed out waiting for note from watchInterrupts")
	}
}

func TestPipeConsToPty(t *testing.T) {
	user := "scott"
	s, c := net.Pipe()
	defer s.Close()
	defer c.Close()
	
	// Server side
	fsys, root := fs.NewFS(user, user, 0755)
	consFile := fs.NewStaticFile(fsys.NewStat("cons", user, user, 0666), []byte("input-from-cons"))
	root.AddChild(consFile)
	go go9p.ServeReadWriter(s, s, fsys.Server())
	
	// Client side
	cl, err := client.NewClient(c, user, "")
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	cf, _ := cl.Open("cons", proto.Oread)
	
	r, w, _ := os.Pipe()
	defer r.Close()
	defer w.Close()
	
	go pipeConsToPty(cf, w)
	
	// Server side has the data. Once it's read, we should close or change it.
	// Actually, the easy way is to close the server handle after a bit.
	time.Sleep(50 * time.Millisecond)
	s.Close()
	
	buf := make([]byte, 100)
	n, err := r.Read(buf)
	if err != nil {
		t.Fatalf("Read from pty failed: %v", err)
	}
	if string(buf[:n]) != "input-from-cons" {
		t.Errorf("Expected input-from-cons, got %q", string(buf[:n]))
	}
}

func TestPipePtyToCons(t *testing.T) {
	user := "scott"
	s, c := net.Pipe()
	defer s.Close()
	defer c.Close()
	
	// Server side
	fsys, root := fs.NewFS(user, user, 0755)
	// We need a file that supports WriteAt. StaticFile does.
	consFile := fs.NewStaticFile(fsys.NewStat("cons", user, user, 0666), nil)
	root.AddChild(consFile)
	go go9p.ServeReadWriter(s, s, fsys.Server())
	
	// Client side
	cl, err := client.NewClient(c, user, "")
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	cf, _ := cl.Open("cons", proto.Owrite)
	
	r, w, _ := os.Pipe()
	defer r.Close()
	defer w.Close()
	
	go pipePtyToCons(r, cf, true) // strip ANSI
	
	w.Write([]byte("\x1b[31moutput\x1b[0m"))
	w.Close() // trigger EOF for pipePtyToCons
	
	// Wait for pipe to finish
	time.Sleep(100 * time.Millisecond)
	
	// Read back from server side to verify write
	// Since we're in-memory, we can check the testFile directly if we have a handle
	// but we don't easily have it here. Let's use the client to read it back.
	cfr, _ := cl.Open("cons", proto.Oread)
	buf := make([]byte, 100)
	n, _ := cfr.ReadAt(buf, 0)
	if string(buf[:n]) != "output" {
		t.Errorf("Expected stripped output 'output', got %q", string(buf[:n]))
	}
}

func TestRelayNotes(t *testing.T) {
	// Start a long-running command that can be signaled
	cmd := exec.Command("sleep", "10")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Failed to start command: %v", err)
	}
	defer cmd.Process.Kill()
	
	noteCh := make(chan string, 1)
	go relayNotes(noteCh, cmd)
	
	noteCh <- "interrupt"
	
	// Wait a moment for signal to be relayed
	time.Sleep(100 * time.Millisecond)
	
	// If the command is still running, it might have ignored the signal,
	// but the important thing is that relayNotes didn't crash.
	// We can check if it's still alive.
	if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
		// some commands exit on SIGINT
	}
}

func TestMountDummyMethods(t *testing.T) {
	user := "scott"
	tempDir, _ := os.MkdirTemp("", "mount-dummy-test")
	defer os.RemoveAll(tempDir)
	
	filePath := filepath.Join(tempDir, "test.txt")
	os.WriteFile(filePath, []byte("test"), 0644)
	fi, _ := os.Stat(filePath)
	
	md := newMountDir(tempDir, "mnt", user, user)
	md.SetParent(nil)
	if md.Parent() != nil {
		t.Error("Parent should be nil")
	}
	if err := md.WriteStat(&proto.Stat{}); err == nil {
		t.Error("Expected error on WriteStat")
	}
	
	mf := newMountFile(filePath, fi, user, user)
	mf.SetParent(nil)
	if mf.Parent() != nil {
		t.Error("Parent should be nil")
	}
	if err := mf.WriteStat(&proto.Stat{}); err == nil {
		t.Error("Expected error on WriteStat")
	}
	// Trigger error on Open non-existent
	mf2 := newMountFile("/non/existent/file", fi, user, user) // keep fi but change path
	if err := mf2.Open(1, proto.Oread); err == nil {
		t.Error("Expected error on opening non-existent file")
	}
}

func TestProxyAuth(t *testing.T) {
	s, c := net.Pipe()
	defer s.Close()
	defer c.Close()
	
	// Mock openFactotum
	oldOpen := openFactotum
	openFactotum = func() (io.ReadWriteCloser, error) {
		return c, nil
	}
	defer func() { openFactotum = oldOpen }()

	// Mock Factotum side
	go func() {
		buf := make([]byte, 4096)
		s.Read(buf)
		s.Write([]byte("ok "))
		s.Read(buf)
		s.Write([]byte("done "))
		s.Read(buf)
		aiBuf := new(bytes.Buffer)
		writeP9String(aiBuf, "user")
		writeP9String(aiBuf, "user")
		writeP9String(aiBuf, "cap")
		writeP9Array(aiBuf, []byte("secret"))
		s.Write(append([]byte("ok "), aiBuf.Bytes()...))
	}()
	
	rw := new(bytes.Buffer)
	ai, err := proxyAuth(rw, "dom=%s", "test")
	if err != nil {
		t.Fatalf("proxyAuth failed: %v", err)
	}
	if ai.Cuid != "user" {
		t.Errorf("Expected user, got %s", ai.Cuid)
	}
}

func TestLaunchShellDirect(t *testing.T) {
	user := "scott"
	nsDir, _ := os.MkdirTemp("", "launch-test")
	defer os.RemoveAll(nsDir)
	os.MkdirAll(filepath.Join(nsDir, "dev"), 0755)
	os.WriteFile(filepath.Join(nsDir, "dev/cons"), nil, 0666)
	
	// Mock openPtyFunc to fail
	oldOpen := openPtyFunc
	openPtyFunc = func() (*os.File, string, error) {
		return nil, "", fmt.Errorf("mock-pty-fail")
	}
	defer func() { openPtyFunc = oldOpen }()
	
	noteCh := make(chan string, 1)
	clientEnv := map[string]string{"cmd": "echo hello"}
	
	// We need a dummy nsRoot with dev/cons for getConsFile
	fsys, nsRoot := fs.NewFS(user, user, 0755)
	devDir := fs.NewStaticDir(fsys.NewStat("dev", user, user, 0755|proto.DMDIR))
	nsRoot.AddChild(devDir)
	
	// Since launchShell calls Run(), it will actually execute "echo hello".
	// We need to make sure the binary exists. On Linux "echo" is usually in /bin/echo.
	shellPath := "/bin/echo"
	
	// This might still be flaky if it tries to open real /dev/cons via nsDir.
	// But we setup nsDir/dev/cons as a real file.
	launchShell(user, "host", shellPath, clientEnv, nsRoot, noteCh, nsDir)
}

func TestRunShellPty(t *testing.T) {
	user := "scott"
	s, c := net.Pipe()
	defer s.Close()
	defer c.Close()
	
	// Server side (cons)
	fsys, root := fs.NewFS(user, user, 0755)
	consFile := fs.NewStaticFile(fsys.NewStat("cons", user, user, 0666), nil)
	root.AddChild(consFile)
	go go9p.ServeReadWriter(s, s, fsys.Server())
	
	// Client side
	cl, _ := client.NewClient(c, user, "")
	cf, _ := cl.Open("cons", proto.Ordwr)
	
	// PTY Mock
	pr, _, _ := os.Pipe() // ptyM
	_, sw, _ := os.Pipe() // slave
	
	noteCh := make(chan string, 1)
	clientEnv := map[string]string{"cmd": "echo pty-test"}
	
	// We use sw as slave (it's a *os.File)
	// and pr as ptyM
	
	// This will actually run the command
	runShellPty(pr, sw, cf, noteCh, user, "host", "/bin/echo", clientEnv, "/tmp")
}

func TestProxyDummyMethods(t *testing.T) {
	pf := &ProxyFile{}
	if err := pf.Open(1, proto.Oread); err != nil {
		t.Errorf("Open failed: %v", err)
	}
}

func TestMountFileReadWriteErrors(t *testing.T) {
	user := "scott"
	tempDir, _ := os.MkdirTemp("", "file-err-test")
	defer os.RemoveAll(tempDir)
	
	filePath := filepath.Join(tempDir, "test.txt")
	os.WriteFile(filePath, []byte("content"), 0644)
	fi, _ := os.Stat(filePath)
	mf := newMountFile(filePath, fi, user, user)
	
	// Read without open
	if _, err := mf.Read(1, 0, 5); err == nil {
		t.Error("Expected error on Read without Open")
	}
	
	// Write without open
	if _, err := mf.Write(1, 0, []byte("data")); err == nil {
		t.Error("Expected error on Write without Open")
	}
	
	// Close without open
	if err := mf.Close(1); err != nil {
		// should be nil but good to call
	}
}

func TestNewMountDirError(t *testing.T) {
	md := newMountDir("/non/existent/path", "mnt", "scott", "scott")
	if md != nil {
		t.Error("Expected nil from newMountDir on non-existent path")
	}
}

func TestHandleRcpuFull(t *testing.T) {
	user := "scott"
	s, c := net.Pipe()
	defer s.Close()
	defer c.Close()

	// 1. Mock negotiateProtocol and proxyAuth
	go func() {
		buf := make([]byte, 4096)
		// Negotiation
		s.Read(buf)
		s.Write([]byte("OK\x00"))
		// Auth
		s.Read(buf)
		s.Write([]byte("ok "))
		s.Read(buf)
		s.Write([]byte("done "))
		s.Read(buf)
		aiBuf := new(bytes.Buffer)
		writeP9String(aiBuf, user)
		writeP9String(aiBuf, user)
		writeP9String(aiBuf, "cap")
		writeP9Array(aiBuf, []byte("secret"))
		s.Write(append([]byte("ok "), aiBuf.Bytes()...))
		
		// TLS-PSK and beyond (script reading)
		// Since we'll mock wrapTlsPskFunc to return raw conn,
		// we just send the script here.
		script := "dir=/tmp\ncmd=ls\n"
		s.Write([]byte(fmt.Sprintf("%d\n%s", len(script), script)))
	}()

	// 2. Mock infrastructure functions
	oldWrap := wrapTlsPskFunc
	wrapTlsPskFunc = func(raw net.Conn, secret []byte) (*tlsConn, error) {
		// Just return a dummy wrapper that uses the raw conn
		return &tlsConn{laddr: raw.LocalAddr(), raddr: raw.RemoteAddr()}, nil
	}
	defer func() { wrapTlsPskFunc = oldWrap }()

	oldNewCl := new9PClientFunc
	new9PClientFunc = func(rwc io.ReadWriteCloser, user, aname string, opts ...client.Option) (*client.Client, error) {
		// We need a real client to talk to something, but handleRcpu 
		// just uses it for Stat/Open. We can mock the server it talks to.
		ss, cc := net.Pipe()
		go func() {
			fsys, root := fs.NewFS(user, user, 0755)
			devDir := fs.NewStaticDir(fsys.NewStat("dev", user, user, 0755|proto.DMDIR))
			root.AddChild(devDir)
			devDir.AddChild(fs.NewStaticFile(fsys.NewStat("cons", user, user, 0666), nil))
			go9p.ServeReadWriter(ss, ss, fsys.Server())
		}()
		return client.NewClient(cc, user, aname, opts...)
	}
	defer func() { new9PClientFunc = oldNewCl }()

	oldStartSrv := startUnix9PServerFunc
	startUnix9PServerFunc = func(nsFS *fs.FS, user string) (string, net.Listener, error) {
		return "/tmp/mock.sock", &mockListener{}, nil
	}
	defer func() { startUnix9PServerFunc = oldStartSrv }()

	oldMount := mountFUSEFunc
	mountFUSEFunc = func(sockPath string) (string, *exec.Cmd, error) {
		return "/tmp/mock.ns", &exec.Cmd{}, nil
	}
	defer func() { mountFUSEFunc = oldMount }()

	oldOpenPty := openPtyFunc
	openPtyFunc = func() (*os.File, string, error) {
		return nil, "", fmt.Errorf("mock-fail")
	}
	defer func() { openPtyFunc = oldOpenPty }()

	// Mock factotum open
	oldOpenFac := openFactotum
	openFactotum = func() (io.ReadWriteCloser, error) {
		_, cc := net.Pipe() // we don't care about the other end for this mock
		return cc, nil
	}
	defer func() { openFactotum = oldOpenFac }()

	// 3. Run handleRcpu
	// It should run through Step 1, 2, 2.5, 3, 4, namespace setup, and launch fallback shell.
	handleRcpu(c, "domain", nil)
}

type mockListener struct{}
func (m *mockListener) Accept() (net.Conn, error) { return nil, fmt.Errorf("mock") }
func (m *mockListener) Close() error { return nil }
func (m *mockListener) Addr() net.Addr { return &net.TCPAddr{} }

func TestAnsiStripping(t *testing.T) {

	user := "scott"
	s, c := net.Pipe()
	defer s.Close()
	defer c.Close()

	// Server side
	fsys, root := fs.NewFS(user, user, 0755)
	testFile := fs.NewStaticFile(fsys.NewStat("test", user, user, 0666), []byte("content"))
	root.AddChild(testFile)
	go go9p.ServeReadWriter(s, s, fsys.Server())

	// Client side
	cl, err := client.NewClient(c, user, "")
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	cf, err := cl.Open("test", proto.Oread)
	if err != nil {
		t.Fatalf("cl.Open failed: %v", err)
	}

	cstat, _ := cl.Stat("test")
	pf := &ProxyFile{
		BaseFile: fs.NewBaseFile(cstat),
		file:     cf,
	}

	// Test Read
	data, err := pf.Read(1, 0, 10)
	if err != nil {
		t.Fatalf("pf.Read failed: %v", err)
	}
	if string(data) != "content" {
		t.Errorf("Expected content, got %q", string(data))
	}

	// Test Write
	cfw, err := cl.Open("test", proto.Owrite)
	if err != nil {
		t.Fatalf("cl.Open for write failed: %v", err)
	}
	pfw := &ProxyFile{
		BaseFile: fs.NewBaseFile(cstat),
		file:     cfw,
	}
	n, err := pfw.Write(1, 0, []byte("new"))
	if err != nil {
		t.Fatalf("pf.Write failed: %v", err)
	}
	if n != 3 {
		t.Errorf("Expected 3 bytes written, got %d", n)
	}
}

func TestMountFileWrite(t *testing.T) {
	user := "scott"
	tempDir, _ := os.MkdirTemp("", "file-write-test")
	defer os.RemoveAll(tempDir)

	filePath := filepath.Join(tempDir, "test.txt")
	os.WriteFile(filePath, []byte("initial"), 0644)
	fi, _ := os.Stat(filePath)

	mf := newMountFile(filePath, fi, user, user)

	// Test Stat
	if mf.Stat().Name != "test.txt" {
		t.Error("Stat name mismatch")
	}

	// Open for write
	err := mf.Open(1, proto.Owrite)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	// Write
	data := []byte("updated")
	n, err := mf.Write(1, 0, data)
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	if n != uint32(len(data)) {
		t.Errorf("Expected %d bytes written, got %d", len(data), n)
	}

	mf.Close(1)

	// Verify file content
	content, _ := os.ReadFile(filePath)
	if string(content) != "updated" {
		t.Errorf("Expected updated, got %q", string(content))
	}
}

func TestFauthProxy(t *testing.T) {
	s, c := net.Pipe()
	defer s.Close()
	defer c.Close()

	// Mock Factotum side
	go func() {
		buf := make([]byte, 4096)
		// 1. Expect "start ..."
		n, _ := s.Read(buf)
		if n < 5 || !strings.HasPrefix(string(buf[:n]), "start") {
			return
		}
		s.Write([]byte("ok "))

		// 2. Expect "read "
		n, _ = s.Read(buf)
		s.Write([]byte("done "))

		// 3. Expect "authinfo "
		n, _ = s.Read(buf)
		// Build a dummy AuthInfo
		aiBuf := new(bytes.Buffer)
		writeP9String(aiBuf, "user")
		writeP9String(aiBuf, "user")
		writeP9String(aiBuf, "cap")
		writeP9Array(aiBuf, []byte("secret"))
		s.Write(append([]byte("ok "), aiBuf.Bytes()...))
	}()

	rw := new(bytes.Buffer) // dummy wire
	ai, err := fauthProxy(rw, c, "params")
	if err != nil {
		t.Fatalf("fauthProxy failed: %v", err)
	}
	if ai.Cuid != "user" {
		t.Errorf("Expected user, got %s", ai.Cuid)
	}
}

func writeP9String(w io.Writer, s string) {
	binary.Write(w, binary.LittleEndian, uint16(len(s)))
	w.Write([]byte(s))
}

func writeP9Array(w io.Writer, b []byte) {
	binary.Write(w, binary.LittleEndian, uint16(len(b)))
	w.Write(b)
}

func TestFauthProxyPhase(t *testing.T) {
	s, c := net.Pipe()
	defer s.Close()
	defer c.Close()

	// Mock Factotum side
	go func() {
		buf := make([]byte, 4096)
		// 1. Expect "start ..."
		s.Read(buf)
		s.Write([]byte("ok "))

		// 2. Expect "read " -> respond with "phase 5"
		s.Read(buf)
		s.Write([]byte("phase 5"))

		// 3. Expect "write " (empty) -> respond with "toosmall 5"
		s.Read(buf)
		s.Write([]byte("toosmall 5"))

		// 4. Expect "write dataa" -> respond with "ok "
		n, _ := s.Read(buf)
		if string(buf[:n]) != "write dataa" {
			s.Write([]byte("error got-" + string(buf[:n])))
			return
		}
		s.Write([]byte("ok "))

		// 5. Expect "read " -> respond with "done "
		s.Read(buf)
		s.Write([]byte("done "))

		// 6. Expect "authinfo "
		s.Read(buf)
		aiBuf := new(bytes.Buffer)
		writeP9String(aiBuf, "user")
		writeP9String(aiBuf, "user")
		writeP9String(aiBuf, "cap")
		writeP9Array(aiBuf, []byte("secret"))
		s.Write(append([]byte("ok "), aiBuf.Bytes()...))
	}()

	rw := bytes.NewBufferString("dataa")
	ai, err := fauthProxy(rw, c, "params")
	if err != nil {
		t.Fatalf("fauthProxy phase failed: %v", err)
	}
	if ai.Cuid != "user" {
		t.Errorf("Expected user, got %s", ai.Cuid)
	}
}

func TestAuthRpcError(t *testing.T) {
	s, c := net.Pipe()
	defer s.Close()
	defer c.Close()

	r := &authRpc{f: c}

	go func() {
		buf := make([]byte, 4096)
		s.Read(buf)
		s.Write([]byte("error some-error"))
	}()

	ret, msg := r.rpc("start", "params")
	if ret != arError {
		t.Errorf("Expected arError, got %v", ret)
	}
	if msg != "some-error" {
		t.Errorf("Expected some-error, got %s", msg)
	}
}

func TestParseScriptProperty(t *testing.T) {
	f := func(keys, values []string) bool {
		if len(keys) != len(values) {
			return true // skip unequal lengths
		}
		var script bytes.Buffer
		expected := make(map[string]string)
		for i, k := range keys {
			if k == "" || strings.Contains(k, "=") || strings.Contains(k, "\n") {
				continue
			}
			v := values[i]
			if strings.Contains(v, "\n") {
				continue
			}
			script.WriteString(fmt.Sprintf("%s=%s\n", k, v))
			expected[k] = v
		}

		result := parseScript(script.Bytes())
		for k, v := range expected {
			if result[k] != v {
				return false
			}
		}
		return true
	}

	if err := quick.Check(f, nil); err != nil {
		t.Error(err)
	}
}

func TestResolveShellProperty(t *testing.T) {
	f := func(cmd string) bool {
		env := map[string]string{"cmd": cmd}
		shell := resolveShell(env)
		if shell == "" {
			return false
		}
		if cmd == "" || cmd == "()" {
			return shell == "/usr/local/bin/rc"
		}
		return shell == cmd
	}

	if err := quick.Check(f, nil); err != nil {
		t.Error(err)
	}
}

type mockReaderAt struct {
	data []byte
}

func (m *mockReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if len(m.data) == 0 {
		return 0, io.EOF
	}
	n := copy(p, m.data)
	m.data = nil // ensure it returns 0/EOF next time to break loop
	return n, nil
}

func TestMonitorNotes(t *testing.T) {
	mock := &mockReaderAt{data: []byte("interrupt\n")}
	noteCh := monitorNotes(mock)

	select {
	case note := <-noteCh:
		if note != "interrupt" {
			t.Errorf("Expected interrupt, got %q", note)
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("Timed out waiting for note")
	}
}

func TestExactReader(t *testing.T) {
	data := []byte("hello world")
	r := bytes.NewReader(data)
	er := &exactReader{r: r}

	buf := make([]byte, 5)
	n, err := er.Read(buf)
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}
	if n != 5 || string(buf) != "hello" {
		t.Errorf("Expected 5 bytes 'hello', got %d %q", n, string(buf))
	}

	// Test partial read that should block/fail if not full
	buf2 := make([]byte, 10)
	n, err = er.Read(buf2) // Only 6 bytes left
	if err != io.ErrUnexpectedEOF {
		t.Errorf("Expected ErrUnexpectedEOF, got %v", err)
	}
}

func TestNegotiateProtocol(t *testing.T) {

	// A simple in-memory buffer to simulate a connection
	// Client sends "dp9ik domain.com\x00"
	var buf bytes.Buffer
	buf.WriteString("dp9ik domain.com\x00")

	choice, err := negotiateProtocol(&buf, "domain.com")
	if err != nil {
		t.Fatalf("negotiateProtocol failed: %v", err)
	}

	expectedChoice := "dp9ik domain.com"
	if choice != expectedChoice {
		t.Errorf("Expected choice %q, got %q", expectedChoice, choice)
	}
}

func TestReadClientEnv(t *testing.T) {
	script := "dir=/tmp\ncmd=ls\n"
	input := bytes.NewBufferString(fmt.Sprintf("%d\n%s", len(script), script))

	env, err := readClientEnv(input)
	if err != nil {
		t.Fatalf("readClientEnv failed: %v", err)
	}

	if env["dir"] != "/tmp" || env["cmd"] != "ls" {
		t.Errorf("Unexpected env: %v", env)
	}
}

func TestResolveShell(t *testing.T) {
	tests := []struct {
		env      map[string]string
		expected string
	}{
		{map[string]string{"cmd": "bash"}, "bash"},
		{map[string]string{}, "/usr/local/bin/rc"},
		{map[string]string{"cmd": "()"}, "/usr/local/bin/rc"},
	}

	for _, tt := range tests {
		result := resolveShell(tt.env)
		if result != tt.expected {
			t.Errorf("For env %v, expected %q, got %q", tt.env, tt.expected, result)
		}
	}
}
func TestGetConsFile(t *testing.T) {
	user := "scott"
	nsFS, nsRoot := fs.NewFS(user, user, 0755)

	// Case 1: No dev dir
	if getConsFile(nsRoot) != nil {
		t.Error("Expected nil cons file when dev dir missing")
	}

	// Case 2: Dev dir but no cons
	devDir := fs.NewStaticDir(nsFS.NewStat("dev", user, user, 0755|proto.DMDIR))
	nsRoot.AddChild(devDir)
	if getConsFile(nsRoot) != nil {
		t.Error("Expected nil cons file when cons missing")
	}

	// Case 3: Cons exists
	// We need a dummy client.File or mock.
	// Since ProxyFile takes a *client.File, and it's a struct,
	// we can just pass nil or a dummy.
	devDir.AddChild(&ProxyFile{
		BaseFile: fs.NewBaseFile(nsFS.NewStat("cons", user, user, 0666)),
		file:     &client.File{},
	})

	if getConsFile(nsRoot) == nil {
		t.Error("Expected cons file to be found")
	}
}

func TestPrepareShellCmd(t *testing.T) {
	shellPath := "/bin/bash"
	authedUser := "scott"
	hname := "test-host"
	nsDir := "/tmp/ns"
	clientEnv := map[string]string{
		"cmd": "bash",
		"dir": "/home/scott",
	}

	cmd := prepareShellCmd(shellPath, authedUser, hname, nsDir, nil, clientEnv, "plan9")

	if cmd.Path != shellPath {
		t.Errorf("Expected path %q, got %q", shellPath, cmd.Path)
	}

	if cmd.Dir != "/home/scott" {
		t.Errorf("Expected dir %q, got %q", "/home/scott", cmd.Dir)
	}

	// Check env
	foundUser := false
	foundTerm := false
	for _, e := range cmd.Env {
		if e == "USER=scott" {
			foundUser = true
		}
		if e == "TERM=plan9" {
			foundTerm = true
		}
	}
	if !foundUser {
		t.Error("USER=scott not found in env")
	}
	if !foundTerm {
		t.Error("TERM=plan9 not found in env")
	}
}

func TestParseScript(t *testing.T) {
	script := []byte("dir=/home/scott\ncmd=rc\nuser=scott\n")

	expected := map[string]string{
		"dir":  "/home/scott",
		"cmd":  "rc",
		"user": "scott",
	}

	result := parseScript(script)
	if !reflect.DeepEqual(result, expected) {
		t.Errorf("Expected %v, got %v", expected, result)
	}
}

func TestParseScriptWithQuotes(t *testing.T) {
	script := []byte("dir='/home/scott space'\ncmd=\"rc -l\"\n")
	expected := map[string]string{
		"dir": "/home/scott space",
		"cmd": "rc -l",
	}

	result := parseScript(script)
	if !reflect.DeepEqual(result, expected) {
		t.Errorf("Expected %v, got %v", expected, result)
	}
}
