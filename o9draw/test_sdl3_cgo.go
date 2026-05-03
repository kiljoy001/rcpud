package main

import (
	"fmt"
	"github.com/jfreymuth/go-sdl3/sdl"
)

func main() {
	if err := sdl.Init(sdl.InitVideo); err != nil {
		fmt.Printf("SDL Init failed: %v\n", err)
		return
	}
	defer sdl.Quit()

	window, err := sdl.CreateWindow("o9 Synthetic Display (SDL3 CGO)", 800, 600, 0)
	if err != nil {
		fmt.Printf("Window creation failed: %v\n", err)
		return
	}
	defer window.Destroy()

	fmt.Println("SDL3 CGO Window opened successfully!")
}
