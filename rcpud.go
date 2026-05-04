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
	"strings"
	"time"

	"github.com/knusbaum/go9p"
	"github.com/knusbaum/go9p/client"
	"github.com/knusbaum/go9p/fs"
	"github.com/knusbaum/go9p/proto"
)

/* 
 * rcpud.go - Master Namespace Server for o9
 */

type ProxyFile struct {
	*fs.BaseFile
	file *client.File
}

func (p *ProxyFile) Open(fid uint64, mode proto.Mode) error {
	return nil
}

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

func (p *ProxyFile) Close(fid uint64) error {
	return nil
}

type rwCloser struct {
	io.Reader
	io.Writer
	io.Closer
}

type stdioConn struct {
	io.ReadCloser
	io.WriteCloser
}

func (s *stdioConn) Read(b []byte) (n int, err error)  { return s.ReadCloser.Read(b) }
func (s *stdioConn) Write(b []byte) (n int, err error) { return s.WriteCloser.Write(b) }
func (s *stdioConn) Close() error                     { s.ReadCloser.Close(); return s.WriteCloser.Close() }
func (s *stdioConn) LocalAddr() net.Addr              { return &net.IPAddr{IP: net.IPv4(127, 0, 0, 1)} }
func (s *stdioConn) RemoteAddr() net.Addr             { return &net.IPAddr{IP: net.IPv4(127, 0, 0, 1)} }
func (s *stdioConn) SetDeadline(t time.Time) error    { return nil }
func (s *stdioConn) SetReadDeadline(t time.Time) error { return nil }
func (s *stdioConn) SetWriteDeadline(t time.Time) error { return nil }

func main() {
	listenAddr := flag.String("l", "", "Listen address (e.g. :17019). If empty, use stdio.")
	flag.Parse()

	log.Printf("rcpud starting with NAMESPACE=%s", os.Getenv("NAMESPACE"))
	factPath := filepath.Join(os.Getenv("NAMESPACE"), "factotum")
	if _, err := os.Stat(factPath); err == nil {
		log.Printf("Found factotum at %s", factPath)
	} else {
		log.Printf("Warning: factotum not found at %s", factPath)
	}

	dom := os.Getenv("AUTH_DOM")
	if dom == "" {
		dom = "rentonsoftworks.coin"
	}

	if *listenAddr != "" {
		var ln net.Listener
		var err error
		ln, err = net.Listen("tcp", *listenAddr)
		if err != nil {
			log.Fatal(err)
		}
		log.Printf("rcpud listening on %s\n", *listenAddr)

		for {
			conn, err := ln.Accept()
			if err != nil {
				log.Print(err)
				continue
			}
			go func(c net.Conn) {
				handleRcpu(c, dom)
			}(conn)
		}
	} else {
		conn := &stdioConn{os.Stdin, os.Stdout}
		handleRcpu(conn, dom)
	}
}

func parseScript(script []byte) map[string]string {
	env := make(map[string]string)
	lines := strings.Split(string(script), "\n")
	for _, line := range lines {
		if strings.Contains(line, "=") {
			parts := strings.SplitN(line, "=", 2)
			key := strings.TrimSpace(parts[0])
			val := strings.Trim(strings.TrimSpace(parts[1]), "'\"")
			env[key] = val
		}
	}
	return env
}

func handleRcpu(conn net.Conn, domain string) {
	defer conn.Close()
	log.Printf("Incoming connection from %s\n", conn.RemoteAddr())

	// Step 1: Offer p9any v2
	log.Printf("Sending offer: v.2 dp9ik@%s", domain)
	fmt.Fprintf(conn, "v.2 dp9ik@%s\x00", domain)

	// Read choice byte-by-byte
	var choiceBuf []byte
	b := make([]byte, 1)
	for {
		_, err := conn.Read(b)
		if err != nil {
			log.Printf("Failed to read choice: %v", err)
			return
		}
		if b[0] == '\x00' {
			break
		}
		choiceBuf = append(choiceBuf, b[0])
	}
	log.Printf("Client choice: %q", string(choiceBuf))

	// Send mandatory OK for v2
	log.Printf("Sending OK confirmation")
	conn.Write([]byte("OK\x00"))

	// Step 2: Auth via fixed proxy (libauth.Proxy has a TCP coalescing bug)
	authSpec := fmt.Sprintf("role=server proto=dp9ik dom=%s", domain)
	log.Printf("Starting auth_proxy with: %s", authSpec)

	ai, err := proxyAuth(conn, authSpec)
	if err != nil {
		log.Printf("Authentication failed: %v", err)
		return
	}

	authedUser := ai.Cuid
	log.Printf("User %s authenticated successfully\n", authedUser)

	// Step 2.5: Upgrade to TLS-PSK using the auth secret
	log.Printf("Upgrading connection to TLS-PSK...")
	tls, err := wrapTlsPsk(conn, ai.Secret)
	if err != nil {
		log.Printf("TLS-PSK upgrade failed: %v", err)
		return
	}
	log.Printf("TLS-PSK connection established.")
	// NOTE: no defer tls.Close() — the go9p client's worker goroutine
	// reads from tls in the background. Closing/freeing SSL while the
	// worker is mid-read causes SIGSEGV. The worker exits when the
	// remote peer closes the connection, and GC cleans up the handle.

	// Step 3: rcpu Handshake — drawterm's rcpu sends script then starts 9P export
	// No FS/\n/OK handshake. The script format is "%7ld\n%s" (7-char fixed-width dec + newline + body)
	reader := bufio.NewReader(tls)

	lenStr, err := reader.ReadString('\n')
	if err != nil {
		log.Printf("Failed to read script length: %v", err)
		return
	}
	var scriptLen int
	fmt.Sscanf(lenStr, "%d", &scriptLen)

	clientEnv := make(map[string]string)
	if scriptLen > 0 {
		scriptBuf := make([]byte, scriptLen)
		_, err = io.ReadFull(reader, scriptBuf)
		if err != nil {
			log.Printf("Failed to read script: %v", err)
			return
		}
		clientEnv = parseScript(scriptBuf)
		log.Printf("Received script from client: %s", string(scriptBuf))
	}

	// Fix script dir if it doesn't exist locally
	if dir, ok := clientEnv["dir"]; ok && dir != "" {
		if _, err := os.Stat(dir); err != nil {
			log.Printf("Client dir %s not found locally, using home dir", dir)
			delete(clientEnv, "dir")
		}
	}

	// Step 4: Client is now serving 9P via exportfs — connect as 9P client
	cl, err := client.NewClient(tls, authedUser, "")
	if err != nil {
		log.Printf("Failed to start 9P client: %v", err)
		return
	}

	nsFS, nsRoot := fs.NewFS(authedUser, authedUser, 0755)
	
	cstat, err := cl.Stat("dev/cons")
	if err == nil {
		cfile, err := cl.Open("dev/cons", proto.Ordwr)
		if err == nil {
			pfile := &ProxyFile{
				BaseFile: fs.NewBaseFile(cstat),
				file:     cfile,
			}
			devDir := fs.NewStaticDir(nsFS.NewStat("dev", authedUser, authedUser, 0755|proto.DMDIR))
			nsRoot.AddChild(devDir)
			devDir.AddChild(pfile)
			log.Printf("Virtual console established.")
		}
	}

	// Initialize gortns agent manager
	gm := NewGortnManager(nsFS, nsRoot, authedUser)
	_ = gm

	// Connect drawsrv synthetic graphics device, if running
	AddDrawProxy(nsFS, nsRoot, authedUser)

	// Use a Unix socket for internal communication
	sockDir, _ := os.MkdirTemp("", "o9.sock.*")
	defer os.RemoveAll(sockDir)
	sockPath := filepath.Join(sockDir, "o9.sock")
	
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		log.Printf("Failed to listen on unix socket: %v", err)
		return
	}
	defer ln.Close()

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil { return }
			go go9p.ServeReadWriter(c, c, nsFS.Server())
		}
	}()

	nsDir, _ := os.MkdirTemp("", "o9.ns.*")
	defer os.RemoveAll(nsDir)

	fuseNs := exec.Command("9pfuse", fmt.Sprintf("unix!%s", sockPath), nsDir)
	fuseNs.Stderr = os.Stderr
	if err := fuseNs.Start(); err != nil {
		log.Printf("Failed to start 9pfuse for namespace: %v", err)
		return
	}
	defer fuseNs.Process.Kill()

	shellPath := "/usr/local/bin/rc"
	if customCmd, ok := clientEnv["cmd"]; ok && customCmd != "" {
		shellPath = customCmd
	}
	if _, err := os.Stat(shellPath); err != nil {
		shellPath = filepath.Join(os.Getenv("HOME"), "Repo/plan9port/o9/l9term")
	}

	time.Sleep(1 * time.Second)

	var cmd *exec.Cmd
	if customCmd, ok := clientEnv["cmd"]; ok && customCmd != "" {
		cmd = exec.Command(shellPath)
	} else {
		cmd = exec.Command(shellPath, "-li")
	}
	
	if dir, ok := clientEnv["dir"]; ok && dir != "" {
		cmd.Dir = dir
	}

	p9bin := "/usr/local/plan9"
	cmd.Env = append(os.Environ(), 
		fmt.Sprintf("PLAN9=%s", p9bin),
		fmt.Sprintf("USER=%s", authedUser),
		fmt.Sprintf("NAMESPACE=%s", nsDir),
		fmt.Sprintf("PATH=%s/bin:%s", p9bin, os.Getenv("PATH")),
		"service=cpu",
	)

	consPath := filepath.Join(nsDir, "dev/cons")
	log.Printf("Attempting to open virtual console at %s", consPath)
	if f, err := os.OpenFile(consPath, os.O_RDWR, 0); err == nil {
		cmd.Stdin = f
		cmd.Stdout = f
		cmd.Stderr = f
		defer f.Close()
	} else {
		log.Printf("Warning: Could not open virtual console at %s: %v", consPath, err)
		null, _ := os.Open(os.DevNull)
		cmd.Stdin = null
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		defer null.Close()
	}

	if err := cmd.Run(); err != nil {
		log.Printf("Shell session failed: %v", err)
	}

	log.Printf("rcpu session closed")
}
