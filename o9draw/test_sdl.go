package main

import (
	"fmt"
	"github.com/veandco/go-sdl2/sdl"
)

func main() {
	if err := sdl.Init(sdl.INIT_EVERYTHING); err != nil {
		fmt.Printf("SDL Init failed: %v\n", err)
		return
	}
	defer sdl.Quit()

	window, err := sdl.CreateWindow("o9 Synthetic Display", sdl.WINDOWPOS_UNDEFINED, sdl.WINDOWPOS_UNDEFINED,
		800, 600, sdl.WINDOW_SHOWN)
	if err != nil {
		fmt.Printf("Window creation failed: %v\n", err)
		return
	}
	defer window.Destroy()

	fmt.Println("SDL2 Window opened successfully!")
}
