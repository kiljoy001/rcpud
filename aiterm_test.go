package main

import (
	"testing"
	"github.com/knusbaum/go9p/fs"
	"github.com/knusbaum/go9p/proto"
)

func TestHandleRoutine(t *testing.T) {
	user := "scott"
	fsys, _ := fs.NewFS("test", user, 0755)
	gortnsDir := fs.NewStaticDir(fsys.NewStat("gortns", user, user, 0755|proto.DMDIR))
	
	cmd := "routine This is a test thought"
	handleInput(cmd, user, fsys, gortnsDir)
	
	children := gortnsDir.Children()
	if len(children) != 1 {
		t.Errorf("Expected 1 child in gortns, got %d", len(children))
	}
}

func TestHandleExit(t *testing.T) {
	user := "scott"
	fsys, _ := fs.NewFS("test", user, 0755)
	gortnsDir := fs.NewStaticDir(fsys.NewStat("gortns", user, user, 0755|proto.DMDIR))

	if handleInput("exit", user, fsys, gortnsDir) {
		t.Error("Expected false for exit command")
	}
	if handleInput("quit", user, fsys, gortnsDir) {
		t.Error("Expected false for quit command")
	}
}
