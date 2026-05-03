package main

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Plan9-Archive/libauth"
	"github.com/knusbaum/go9p"
	"github.com/knusbaum/go9p/client"
	"github.com/knusbaum/go9p/fs"
	"github.com/knusbaum/go9p/proto"
)

/* 
 * rcpud_tls.go - Master Namespace Server for o9 with TLS support.
 * Uses the broker certificates from 9lx.
 */

type ProxyFile struct {
	*fs.BaseFile
	file *client.File
}

func (p *ProxyFile) Open(fid uint64, mode proto.Mode) error { return nil }
func (p *ProxyFile) Read(fid uint64, offset uint64, count uint64) ([]byte, error) {
	buf := make([]byte, count)
	n, err := p.file.ReadAt(buf, int64(offset))
	if err != nil && err != io.EOF { return nil, err }
	return buf[:n], nil
}
func (p *ProxyFile) Write(fid uint64, offset uint64, data []byte) (uint32, error) {
	n, err := p.file.WriteAt(data, int64(offset))
	return uint32(n), err
}
func (p *ProxyFile) Close(fid uint64) error { return nil }

type rwCloser struct {
	io.Reader
	io.Writer
	io.Closer
}

func main() {
	certPath := "/home/scott/Repo/9lx/initramfs/etc/9plx/broker.crt"
	keyPath := "/home/scott/Repo/9lx/initramfs/etc/9plx/broker.key"

	cer, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		log.Fatalf("Failed to load certificates: %v", err)
	}

	config := &tls.Config{Certificates: []tls.Certificate{cer}}
	ln, err := tls.Listen("tcp", "0.0.0.0:17019", config)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("AI Master Node (TLS-enabled) listening on :17019")

	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Print(err)
			continue
		}
		go handleRcpu(conn)
	}
}

func handleRcpu(conn net.Conn) {
	defer conn.Close()
	fmt.Printf("Incoming TLS connection from %s\n", conn.RemoteAddr())

	// Step 1: Auth via local Factotum
	ai, err := libauth.Proxy(conn, "role=server proto=dp9ik dom=rentonsoftworks.coin")
	if err != nil {
		log.Printf("Authentication failed: %v", err)
		return
	}
	authedUser := ai.Cuid
	fmt.Printf("User %s authenticated successfully.\n", authedUser)

	reader := bufio.NewReader(conn)
	
	lenStr, _ := reader.ReadString('\n')
	var scriptLen int
	fmt.Sscanf(lenStr, "%d", &scriptLen)
	
	clientEnv := make(map[string]string)
	if scriptLen > 0 {
		scriptBuf := make([]byte, scriptLen)
		io.ReadFull(reader, scriptBuf)
		lines := strings.Split(string(scriptBuf), "\n")
		for _, line := range lines {
			if strings.Contains(line, "=") {
				parts := strings.SplitN(line, "=", 2)
				clientEnv[strings.TrimSpace(parts[0])] = strings.Trim(strings.TrimSpace(parts[1]), "'\"")
			}
		}
	}
	
	conn.Write([]byte("FS\n"))
	conn.Write([]byte("/\n"))

	okBuf := make([]byte, 2)
	conn.Read(okBuf)
	if string(okBuf) != "OK" {
		return
	}

	cl, err := client.NewClient(conn, authedUser, "")
	if err != nil {
		log.Printf("Failed to start 9P client: %v", err)
		return
	}

	nsFS, _ := fs.NewFS("o9", authedUser, 0755)
	cstat, err := cl.Stat("dev/cons")
	if err == nil {
		cfile, err := cl.Open("dev/cons", proto.Ordwr)
		if err == nil {
			pfile := &ProxyFile{
				BaseFile: fs.NewBaseFile(cstat),
				file:     cfile,
			}
			devDir := fs.NewStaticDir(nsFS.NewStat("dev", authedUser, authedUser, 0755|proto.DMDIR))
			root := nsFS.Root.(*fs.StaticDir)
			root.AddChild(devDir)
			devDir.AddChild(pfile)
		}
	}

	nsConn1, nsConn2 := net.Pipe()
	go go9p.ServeReadWriter(nsConn1, nsConn1, nsFS.Server())

	nsDir, _ := os.MkdirTemp("", "o9.ns.*")
	defer os.RemoveAll(nsDir)

	fuseNs := exec.Command("9pfuse", "-", nsDir)
	fuseNs.Stdin = nsConn2
	fuseNs.Stdout = nsConn2
	if err := fuseNs.Start(); err != nil {
		log.Printf("Failed to start 9pfuse: %v", err)
		return
	}
	defer fuseNs.Process.Kill()

	shellPath := "/usr/local/bin/rc"
	if _, err := os.Stat(shellPath); err != nil {
		shellPath = filepath.Join(os.Getenv("HOME"), "Repo/plan9port/o9/aiterm")
	}

	time.Sleep(500 * time.Millisecond)

	cmd := exec.Command(shellPath, "-li")
	p9bin := "/usr/local/plan9"
	cmd.Env = append(os.Environ(), 
		fmt.Sprintf("PLAN9=%s", p9bin),
		fmt.Sprintf("USER=%s", authedUser),
		fmt.Sprintf("NAMESPACE=%s", nsDir),
		fmt.Sprintf("PATH=%s/bin:%s", p9bin, os.Getenv("PATH")),
		"service=cpu",
	)

	consPath := filepath.Join(nsDir, "dev/cons")
	if f, err := os.OpenFile(consPath, os.O_RDWR, 0); err == nil {
		cmd.Stdin = f
		cmd.Stdout = f
		cmd.Stderr = f
		defer f.Close()
	} else {
		null, _ := os.Open(os.DevNull)
		cmd.Stdin = null
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		defer null.Close()
	}

	if err := cmd.Run(); err != nil {
		log.Printf("Shell session failed: %v", err)
	}

	fmt.Println("rcpu session closed")
}
