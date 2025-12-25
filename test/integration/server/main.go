package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"time"
)

func main() {
	// Wait for tailscale0
	for range 60 {
		if iface, err := net.InterfaceByName("tailscale0"); err == nil {
			if addrs, err := iface.Addrs(); err == nil && len(addrs) > 0 {
				if ipnet, ok := addrs[0].(*net.IPNet); ok {
					fmt.Printf("Tailscale IP: %s\n", ipnet.IP)
					break
				}
			}
		}
		time.Sleep(time.Second)
	}

	// Start echo server
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", ":9998")
	if err != nil {
		fmt.Printf("FATAL: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("Echo server listening on :9998")

	for {
		conn, err := ln.Accept()
		if err != nil {
			continue
		}
		go func(c net.Conn) {
			defer c.Close()
			_, _ = io.Copy(c, c)
		}(conn)
	}
}
