package main

import (
	"fmt"
	"io"
	"net"
	"github.com/Plan9-Archive/libauth"
)

/* 
 * rcpud.go - Master Namespace Server for o9
 * Fixed Handshake for 9front.
 */

func main() {
	ln, err := net.Listen("tcp", "0.0.0.0:17019")
	if err != nil { panic(err) }
	fmt.Println("AI Master Node listening on :17019")
	for {
		conn, err := ln.Accept()
		if err != nil { continue }
		go handle(conn)
	}
}

func handle(conn net.Conn) {
	defer conn.Close()
	fmt.Printf("Connection from %s\n", conn.RemoteAddr())

	// Force the dp9ik offer immediately to satisfy 9front rcpu
	fmt.Fprintf(conn, "dp9ik@rentonsoftworks.coin\x00")

	ai, err := libauth.Proxy(conn, "role=server proto=dp9ik dom=rentonsoftworks.coin")
	if err != nil {
		fmt.Printf("Auth failed: %v\n", err)
		return
	}
	fmt.Printf("User %s authenticated.\n", ai.Cuid)

	// ... remainder of rcpud logic ...
}
