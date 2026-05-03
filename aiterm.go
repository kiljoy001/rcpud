package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/knusbaum/go9p/fs"
	"github.com/knusbaum/go9p/proto"
)

/* 
 * aiterm.go - The 9P-Native AI Agent Shell (Wrapper for rc)
 */

func main() {
	fmt.Println("AI TERM (9P-Native Agent Shell) Online.")
	
	// Create our own internal 9P namespace for Gortns
	user := os.Getenv("USER")
	if user == "" { user = "scott" }
	fsys, groot := fs.NewFS("aiterm", user, 0755)
	gortnsDir := fs.NewStaticDir(fsys.NewStat("gortns", user, user, 0755|proto.DMDIR))
	groot.AddChild(gortnsDir)

	// Main REPL Loop
	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Print("aiterm% ")
		input, err := reader.ReadString('\n')
		if err != nil {
			break
		}
		
		cmdStr := strings.TrimSpace(input)
		if !handleInput(cmdStr, user, fsys, gortnsDir) {
			break
		}
	}
}

func handleInput(cmdStr string, user string, fsys *fs.FS, gortnsDir *fs.StaticDir) bool {
	if cmdStr == "" {
		return true
	}

	// Handle AI-Native commands
	if strings.HasPrefix(cmdStr, "routine ") {
		spawnGortn(fsys, gortnsDir, cmdStr[8:], user)
		return true
	}

	if cmdStr == "exit" || cmdStr == "quit" {
		return false
	}

	// Fallback to rc
	executeInRc(cmdStr)
	return true
}

func spawnGortn(fsys *fs.FS, dir *fs.StaticDir, thought, user string) {
	fmt.Printf("[Gortn Spawned]: %s\n", thought)
	id := fmt.Sprintf("t%d", os.Getpid())
	dir.AddChild(fs.NewStaticFile(fsys.NewStat(id, user, user, 0444), []byte(thought)))
}

func executeInRc(cmdStr string) {
	shell := "/usr/local/plan9/bin/rc"
	if _, err := os.Stat(shell); err != nil {
		shell = "/bin/sh" // Ultimate fallback
	}
	
	cmd := exec.Command(shell, "-c", cmdStr)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	
	if err := cmd.Run(); err != nil {
		fmt.Printf("[Shell Error]: %v\n", err)
	}
}
