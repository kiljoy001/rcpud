//go:build rcpud

package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/knusbaum/go9p"
	"github.com/knusbaum/go9p/client"
	"github.com/knusbaum/go9p/fs"
	"github.com/knusbaum/go9p/proto"
)

var buildInfo = fmt.Sprintf("rcpud/%s", hostnameOrDie())

func hostnameOrDie() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return h
}

// ANSI escape sequence matcher — strips color/movement codes so drawterm
// renders clean text instead of raw escape bytes.
// Covers SGR ("[31m"), cursor movement ("[2A"), erase ("[2J"), OSC ("]0;..."),
// and DECTCEM ("[?25h" / "[?25l").
var ansiRegexp = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]|\x1b\][0-9;]*[^\x1b\x07]*(\x07|\x1b\\)|\x1b[PX^_]`)

type mountSpec struct {
	Name string
	Path string
}

type ProxyFile struct {
	*fs.BaseFile
	file *client.File
}

func (p *ProxyFile) Open(fid uint64, mode proto.Mode) error { return nil }

func (p *ProxyFile) Read(fid uint64, offset uint64, count uint64) ([]byte, error) {
	buf := make([]byte, count)
	n, err := p.file.ReadAt(buf, int64(offset))
	if err != nil && err != io.EOF {
		return nil, err
	}
	return buf[:n], nil
}

func (p *ProxyFile) Write(fid uint64, offset uint64, data []byte) (uint32, error) {
	n, err := p.file.WriteAt(data, int64(offset))
	return uint32(n), err
}

func (p *ProxyFile) Close(fid uint64) error { return nil }

type stdioConn struct {
	io.ReadCloser
	io.WriteCloser
}

func (s *stdioConn) Read(b []byte) (int, error)  { return s.ReadCloser.Read(b) }
func (s *stdioConn) Write(b []byte) (int, error) { return s.WriteCloser.Write(b) }
func (s *stdioConn) Close() error {
	s.ReadCloser.Close()
	return s.WriteCloser.Close()
}
func (s *stdioConn) LocalAddr() net.Addr                { return &net.IPAddr{IP: net.IPv4(127, 0, 0, 1)} }
func (s *stdioConn) RemoteAddr() net.Addr               { return &net.IPAddr{IP: net.IPv4(127, 0, 0, 1)} }
func (s *stdioConn) SetDeadline(t time.Time) error      { return nil }
func (s *stdioConn) SetReadDeadline(t time.Time) error  { return nil }
func (s *stdioConn) SetWriteDeadline(t time.Time) error { return nil }

var stripANSI bool
var drawsrvAddr string

func main() {
	listenAddr := flag.String("l", "", "Listen address. If empty, use stdio.")
	var mountFlags mountsFlag
	flag.BoolVar(&stripANSI, "no-strip", false, "Disable ANSI escape stripping (show raw codes)")
	flag.StringVar(&drawsrvAddr, "drawsrv", "", "drawsrv TCP address (e.g. :17029) for framebuffer export")
	flag.Var(&mountFlags, "mount", "Directory to serve: path or name=path (repeatable)")
	flag.Parse()

	log.Printf("rcpud/%s starting", buildInfo)
	if drawsrvAddr != "" {
		log.Printf("drawsrv framebuffer at %s", drawsrvAddr)
	}
	if !stripANSI {
		log.Printf("ANSI escape stripping enabled (use -no-strip to show raw codes)")
	}
	nsPath := os.Getenv("NAMESPACE")
	if nsPath == "" {
		nsPath = "/tmp/ns." + os.Getenv("USER")
		os.Setenv("NAMESPACE", nsPath)
	}
	if _, err := os.Stat(filepath.Join(nsPath, "factotum")); err != nil {
		log.Printf("Warning: factotum not at %s", filepath.Join(nsPath, "factotum"))
	}

	dom := os.Getenv("AUTH_DOM")
	if dom == "" {
		dom = "rentonsoftworks.coin"
	}

	if *listenAddr != "" {
		ln, err := net.Listen("tcp", *listenAddr)
		if err != nil {
			log.Fatal(err)
		}
		log.Printf("Listening on %s", *listenAddr)
		for {
			conn, err := ln.Accept()
			if err != nil {
				log.Print(err)
				continue
			}
			go func(c net.Conn) { handleRcpu(c, dom, mountFlags) }(conn)
		}
	} else {
		handleRcpu(&stdioConn{os.Stdin, os.Stdout}, dom, mountFlags)
	}
}

type mountsFlag []mountSpec

func (m *mountsFlag) String() string { return fmt.Sprint([]mountSpec(*m)) }
func (m *mountsFlag) Set(v string) error {
	var s mountSpec
	if i := strings.Index(v, "="); i > 0 {
		s.Name = v[:i]
		s.Path = v[i+1:]
	} else {
		s.Name = filepath.Base(v)
		s.Path = v
	}
	abs, err := filepath.Abs(s.Path)
	if err != nil {
		return fmt.Errorf("bad mount path %q: %w", s.Path, err)
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return fmt.Errorf("mount path %q: %w", abs, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("mount path %q is not a directory", abs)
	}
	s.Path = abs
	*m = append(*m, s)
	return nil
}

func parseScript(script []byte) map[string]string {
	env := make(map[string]string)
	for _, line := range strings.Split(string(script), "\n") {
		if strings.Contains(line, "=") {
			parts := strings.SplitN(line, "=", 2)
			env[strings.TrimSpace(parts[0])] = strings.Trim(strings.TrimSpace(parts[1]), "'\"")
		}
	}
	return env
}

var (
	wrapTlsPskFunc        func(net.Conn, []byte) (net.Conn, error) = func(raw net.Conn, secret []byte) (net.Conn, error) {
		return wrapTlsPsk(raw, secret)
	}
	new9PClientFunc       = client.NewClient
	startUnix9PServerFunc = startUnix9PServer
	mountFUSEFunc         = mountFUSE
)

func handleRcpu(conn net.Conn, domain string, mounts []mountSpec) {
	defer conn.Close()
	hname, _ := os.Hostname()
	if hname == "" {
		hname = "unknown"
	}
	log.Printf("Incoming connection from %s", conn.RemoteAddr())

	// Step 1: Offer p9any v2
	choice, err := negotiateProtocol(conn, domain)
	if err != nil {
		log.Printf("Negotiation failed: %v", err)
		return
	}
	log.Printf("Client choice: %q", choice)

	// Step 2: Auth
	ai, err := proxyAuth(conn, "role=server proto=dp9ik dom=%s", domain)
	if err != nil {
		log.Printf("Authentication failed: %v", err)
		return
	}
	authedUser := ai.Cuid
	log.Printf("User %s authenticated successfully", authedUser)

	// Step 2.5: TLS-PSK
	tls, err := wrapTlsPskFunc(conn, ai.Secret)
	if err != nil {
		log.Printf("TLS-PSK upgrade failed: %v", err)
		return
	}
	log.Printf("TLS-PSK connection established")

	// Step 3: Read script
	clientEnv, err := readClientEnv(tls)
	if err != nil {
		log.Printf("Failed to read client env: %v", err)
		return
	}

	// Step 4: Connect 9P client to drawterm's exportfs
	cl, err := new9PClientFunc(tls, authedUser, "")
	if err != nil {
		log.Printf("Failed to start 9P client: %v", err)
		return
	}

	// Build local namespace with virtual dev/cons proxy
	nsFS, nsRoot := setupNamespace(cl, authedUser)

	// Watch mnt/cpunote/data for Delete (interrupt) notes
	noteCh := watchInterrupts(cl)

	// Add mounts
	for _, m := range mounts {
		log.Printf("Mounting %q at /%s", m.Path, m.Name)
		nsRoot.AddChild(newMountDir(m.Path, m.Name, authedUser, authedUser))
	}

	// Start local 9P server (FUSE-accessible)
	sockPath, ln, err := startUnix9PServerFunc(nsFS, authedUser)
	if err != nil {
		log.Printf("Failed to start 9P server: %v", err)
		return
	}
	defer ln.Close()
	defer os.RemoveAll(filepath.Dir(sockPath))

	nsDir, fuseNs, err := mountFUSEFunc(sockPath)
	if err != nil {
		log.Printf("Failed to mount FUSE: %v", err)
		return
	}
	defer os.RemoveAll(nsDir)
	defer func() {
		if fuseNs != nil && fuseNs.Process != nil {
			fuseNs.Process.Kill()
		}
	}()

	// Determine shell
	shellPath := resolveShell(clientEnv)

	launchShell(authedUser, hname, shellPath, clientEnv, nsRoot, noteCh, nsDir)
}

var openPtyFunc = openPty

func launchShell(authedUser, hname, shellPath string, clientEnv map[string]string, nsRoot *fs.StaticDir, noteCh chan string, nsDir string) {
	// Try pty for job control. Fall back to direct cons if it fails.
	ptyM, slaveName, err := openPtyFunc()
	if err == nil {
		defer ptyM.Close()
		// Set terminal size
		var ws struct {
			Row, Col, X, Y uint16
		}
		ws.Row, ws.Col = 24, 80
		syscall.Syscall(syscall.SYS_IOCTL, ptyM.Fd(), syscall.TIOCSWINSZ, uintptr(unsafe.Pointer(&ws)))

		// Try opening the slave with a short retry
		var slave *os.File
		for i := 0; i < 5; i++ {
			slave, err = os.OpenFile(slaveName, os.O_RDWR, 0)
			if err == nil {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if err == nil {
			defer slave.Close()
			runShellPty(ptyM, slave, getConsFile(nsRoot), noteCh, authedUser, hname, shellPath, clientEnv, nsDir)
			return
		}
		log.Printf("Pty slave open failed (%v), falling back to direct cons", err)
		ptyM.Close()
	}

	log.Printf("Using direct console (no pty - job control unavailable)")
	runShellDirect(getConsFile(nsRoot), noteCh, authedUser, hname, shellPath, clientEnv, nsDir)
}

func setupNamespace(cl *client.Client, user string) (*fs.FS, *fs.StaticDir) {
	nsFS, nsRoot := fs.NewFS(user, user, 0755)
	if cstat, err := cl.Stat("dev/cons"); err == nil {
		if cfile, err := cl.Open("dev/cons", proto.Ordwr); err == nil {
			devDir := fs.NewStaticDir(nsFS.NewStat("dev", user, user, 0755|proto.DMDIR))
			devDir.AddChild(&ProxyFile{BaseFile: fs.NewBaseFile(cstat), file: cfile})
			nsRoot.AddChild(devDir)
			log.Printf("Virtual console established")
		}
	}
	return nsFS, nsRoot
}

func getConsFile(root *fs.StaticDir) *client.File {
	dev := root.Children()["dev"]
	if dev == nil {
		return nil
	}
	if d, ok := dev.(*fs.StaticDir); ok {
		cons := d.Children()["cons"]
		if cons == nil {
			return nil
		}
		if p, ok := cons.(*ProxyFile); ok {
			return p.file
		}
	}
	return nil
}

type ReadAtCloser interface {
	io.ReaderAt
	io.Closer
}

func watchInterrupts(cl *client.Client) chan string {
	if cpunote, err := cl.Open("mnt/cpunote/data", proto.Ordwr); err == nil {
		return monitorNotes(cpunote)
	}
	return make(chan string, 4)
}

func monitorNotes(r io.ReaderAt) chan string {
	noteCh := make(chan string, 4)
	go func() {
		buf := make([]byte, 256)
		for {
			n, err := r.ReadAt(buf, 0)
			if err != nil || n == 0 {
				return
			}
			noteCh <- strings.TrimSpace(string(buf[:n]))
		}
	}()
	return noteCh
}

func startUnix9PServer(nsFS *fs.FS, user string) (string, net.Listener, error) {
	sockDir, _ := os.MkdirTemp("", "o9.sock.*")
	sockPath := filepath.Join(sockDir, "o9.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		return "", nil, err
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go go9p.ServeReadWriter(c, c, nsFS.Server())
		}
	}()
	return sockPath, ln, nil
}

func mountFUSE(sockPath string) (string, *exec.Cmd, error) {
	nsDir, _ := os.MkdirTemp("", "o9.ns.*")
	cmd := exec.Command("9pfuse", fmt.Sprintf("unix!%s", sockPath), nsDir)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return "", nil, err
	}
	return nsDir, cmd, nil
}

func negotiateProtocol(conn io.ReadWriter, domain string) (string, error) {
	fmt.Fprintf(conn, "v.2 dp9ik@%s\x00", domain)
	var choiceBuf []byte
	b := make([]byte, 1)
	for {
		if _, err := conn.Read(b); err != nil {
			return "", fmt.Errorf("read choice: %w", err)
		}
		if b[0] == '\x00' {
			break
		}
		choiceBuf = append(choiceBuf, b[0])
	}
	conn.Write([]byte("OK\x00"))
	return string(choiceBuf), nil
}

func readClientEnv(r io.Reader) (map[string]string, error) {
	reader := bufio.NewReader(r)
	lenStr, err := reader.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("read script length: %w", err)
	}
	var scriptLen int
	fmt.Sscanf(lenStr, "%d", &scriptLen)
	clientEnv := make(map[string]string)
	if scriptLen > 0 {
		scriptBuf := make([]byte, scriptLen)
		if _, err = io.ReadFull(reader, scriptBuf); err != nil {
			return nil, fmt.Errorf("read script: %w", err)
		}
		clientEnv = parseScript(scriptBuf)
	}
	if dir, ok := clientEnv["dir"]; ok && dir != "" {
		if _, err := os.Stat(dir); err != nil {
			delete(clientEnv, "dir")
		}
	}
	return clientEnv, nil
}

func resolveShell(env map[string]string) string {
	shellPath := "/usr/local/bin/rc"
	if c, ok := env["cmd"]; ok && c != "" && c != "()" {
		shellPath = c
	}
	return shellPath
}
func runShellPty(ptyM, slave *os.File, consFile *client.File, noteCh chan string,
	authedUser, hname, shellPath string, clientEnv map[string]string, nsDir string) {

	cmd := prepareShellCmd(shellPath, authedUser, hname, nsDir, slave, clientEnv, "plan9")

	if err := cmd.Start(); err != nil {
		log.Printf("Failed to start shell: %v", err)
		return
	}

	// cons -> pty (user input to shell)
	if consFile != nil {
		go pipeConsToPty(consFile, ptyM)
		// pty -> cons (shell output to drawterm, with optional ANSI strip)
		go pipePtyToCons(ptyM, consFile, !stripANSI)
	}

	// Note watcher: Delete -> SIGINT to shell's process group
	go relayNotes(noteCh, cmd)

	log.Printf("Starting shell for %s on %s (pty)", authedUser, hname)
	if err := cmd.Wait(); err != nil {
		log.Printf("Shell session failed: %v", err)
	}
	log.Printf("Session closed for %s", authedUser)
}

func prepareShellCmd(shellPath, authedUser, hname, nsDir string, slave *os.File, clientEnv map[string]string, term string) *exec.Cmd {
	var cmd *exec.Cmd
	if c, ok := clientEnv["cmd"]; ok && c != "" && c != "()" {
		cmd = exec.Command(shellPath)
	} else {
		cmd = exec.Command(shellPath, "-li")
	}
	if d, ok := clientEnv["dir"]; ok && d != "" {
		cmd.Dir = d
	}
	cmd.Stdin = slave
	cmd.Stdout = slave
	cmd.Stderr = slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("hostname=%s", hname),
		fmt.Sprintf("USER=%s", authedUser),
		fmt.Sprintf("NAMESPACE=%s", nsDir),
		fmt.Sprintf("PLAN9=/usr/local/plan9"),
		fmt.Sprintf("PATH=/usr/local/plan9/bin:%s", os.Getenv("PATH")),
		"service=cpu",
	)
	if term != "" {
		cmd.Env = append(cmd.Env, fmt.Sprintf("TERM=%s", term))
	}
	return cmd
}

func pipeConsToPty(consFile *client.File, ptyM *os.File) {
	buf := make([]byte, 4096)
	var offset uint64
	for {
		n, err := consFile.ReadAt(buf, int64(offset))
		if err != nil || n == 0 {
			return
		}
		ptyM.Write(buf[:n])
		offset += uint64(n)
	}
}

func pipePtyToCons(ptyM *os.File, consFile *client.File, strip bool) {
	buf := make([]byte, 4096)
	for {
		n, err := ptyM.Read(buf)
		if err != nil {
			return
		}
		if n > 0 {
			data := buf[:n]
			if strip {
				data = ansiRegexp.ReplaceAll(data, []byte{})
			}
			consFile.WriteAt(data, 0)
		}
	}
}

func relayNotes(noteCh chan string, cmd *exec.Cmd) {
	for note := range noteCh {
		switch note {
		case "interrupt", "del":
			pgid, err := syscall.Getpgid(cmd.Process.Pid)
			if err == nil && pgid > 0 {
				syscall.Kill(-pgid, syscall.SIGINT)
				log.Printf("Relayed %s note as SIGINT to PGID %d", note, pgid)
			}
		}
	}
}

func runShellDirect(consFile *client.File, noteCh chan string,
	authedUser, hname, shellPath string, clientEnv map[string]string, nsDir string) {

	consPath := filepath.Join(nsDir, "dev/cons")
	consIO, err := os.OpenFile(consPath, os.O_RDWR, 0)
	if err != nil {
		log.Printf("Could not open cons at %s: %v", consPath, err)
		return
	}
	defer consIO.Close()

	cmd := prepareShellCmd(shellPath, authedUser, hname, nsDir, consIO, clientEnv, "")

	log.Printf("Starting shell for %s on %s (direct cons)", authedUser, hname)
	if err := cmd.Run(); err != nil {
		log.Printf("Shell session failed: %v", err)
	}
	log.Printf("Session closed for %s", authedUser)
}

func openPty() (*os.File, string, error) {
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		return nil, "", err
	}
	var unlock int32 = 0
	syscall.Syscall(syscall.SYS_IOCTL, m.Fd(), syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock)))
	var n int
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, m.Fd(), syscall.TIOCGPTN, uintptr(unsafe.Pointer(&n))); e != 0 {
		m.Close()
		return nil, "", fmt.Errorf("TIOCGPTN: %v", e)
	}
	return m, fmt.Sprintf("/dev/pts/%d", n), nil
}
