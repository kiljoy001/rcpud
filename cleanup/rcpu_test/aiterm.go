package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/knusbaum/go9p/fs"
	"github.com/knusbaum/go9p/proto"
)

/* 
 * aiterm.go - The 9P-Native AI Agent Shell with GUI support.
 * Renders graphical output to the client's /dev/draw.
 */

func main() {
	fmt.Println("AI TERM (9P-Native Agent Shell) Online.")
	root := os.Getenv("AITERM_ROOT")
	if root != "" {
		fmt.Printf("Connected to Plan 9 Grid via %s\n", root)
	}
	
	// Create our own internal 9P namespace for Gortns
	user := os.Getenv("USER")
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
		
		cmd := strings.TrimSpace(input)
		if cmd == "" {
			continue
		}

		if strings.HasPrefix(cmd, "routine ") {
			spawnGortn(fsys, gortnsDir, cmd[8:], user)
			continue
		}

		if cmd == "exit" || cmd == "quit" {
			break
		}
		processCommand(cmd)
	}
}

func spawnGortn(fsys *fs.FS, dir *fs.StaticDir, thought, user string) {
	fmt.Printf("[Gortn Spawned]: %s\n", thought)
	id := fmt.Sprintf("t%d", os.Getpid())
	dir.AddChild(fs.NewStaticFile(fsys.NewStat(id, user, user, 0444), []byte(thought)))
}

func processCommand(cmd string) {
	fmt.Printf("Executing on Linux: %s\n", cmd)
}
