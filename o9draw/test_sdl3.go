package main

import (
	"fmt"
	"github.com/Zyko0/go-sdl3/sdl"
)

func main() {
	if err := sdl.Init(sdl.INIT_VIDEO); err != nil {
		fmt.Printf("SDL Init failed: %v\n", err)
		return
	}
	defer sdl.Quit()

	window, renderer, err := sdl.CreateWindowAndRenderer("o9 Synthetic Display (SDL3)", 800, 600, 0)
	if err != nil {
		fmt.Printf("Window/Renderer creation failed: %v\n", err)
		return
	}
	defer window.Destroy()
	defer renderer.Destroy()

	fmt.Println("SDL3 Window opened successfully!")
}
