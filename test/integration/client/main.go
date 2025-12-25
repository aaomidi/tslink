package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

var (
	serverHostname = os.Getenv("SERVER_HOSTNAME")
	staticService  = os.Getenv("STATIC_SERVICE")
	tailnetSuffix  = os.Getenv("TAILNET_SUFFIX")
)

func main() {
	if serverHostname == "" {
		serverHostname = "tslink-server"
	}
	if staticService == "" {
		staticService = "tslink-test.atlas-diminished.ts.net"
	}
	if tailnetSuffix == "" {
		tailnetSuffix = "atlas-diminished.ts.net"
	}

	fmt.Printf("Server: %s\nStatic service: %s\n", serverHostname, staticService)

	// Wait for tailscale0
	fmt.Println("\n[Setup] Waiting for Tailscale...")
	for range 60 {
		if iface, err := net.InterfaceByName("tailscale0"); err == nil {
			if addrs, err := iface.Addrs(); err == nil && len(addrs) > 0 {
				if ipnet, ok := addrs[0].(*net.IPNet); ok {
					fmt.Printf("Our IP: %s\n", ipnet.IP)
					break
				}
			}
		}
		time.Sleep(time.Second)
	}

	// Wait for server
	fmt.Println("\n[Setup] Waiting for server (20s)...")
	time.Sleep(20 * time.Second)

	results := make(map[string]bool)

	// Test 1: Direct connection via hostname
	fqdn := serverHostname + "." + tailnetSuffix
	fmt.Printf("\n=== TEST 1: Direct echo (%s:9998) ===\n", fqdn)
	results["direct_echo"] = testEcho(fqdn, 9998)

	// Test 2a: Short hostname resolves
	fmt.Printf("\n=== TEST 2a: Short hostname (%s) ===\n", serverHostname)
	results["short_hostname"] = testResolve(serverHostname)

	// Test 2b: FQDN resolves
	fmt.Printf("\n=== TEST 2b: FQDN (%s) ===\n", fqdn)
	results["fqdn"] = testResolve(fqdn)

	// Test 3a: Static service short
	serviceShort := strings.Split(staticService, ".")[0]
	fmt.Printf("\n=== TEST 3a: Service short (%s) ===\n", serviceShort)
	results["service_short"] = testResolve(serviceShort)

	// Test 3b: Static service FQDN + connect
	fmt.Printf("\n=== TEST 3b: Service FQDN (%s:9999) ===\n", staticService)
	results["service_fqdn"] = testConnect(staticService, 9999)

	// Summary
	passed := 0
	for _, ok := range results {
		if ok {
			passed++
		}
	}
	fmt.Printf("\n=== SUMMARY: %d/%d ===\n", passed, len(results))
	for name, ok := range results {
		mark := "FAIL"
		if ok {
			mark = "PASS"
		}
		fmt.Printf("  %s: %s\n", name, mark)
	}

	if !results["direct_echo"] {
		os.Exit(1)
	}
}

func testEcho(host string, port int) bool {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	for range 5 {
		ctx := context.Background()
		conn, err := dialer.DialContext(ctx, "tcp", fmt.Sprintf("%s:%d", host, port))
		if err != nil {
			time.Sleep(time.Second)
			continue
		}
		defer conn.Close()

		msg := "hello-test"
		_, _ = conn.Write([]byte(msg))
		buf := make([]byte, 1024)
		n, _ := conn.Read(buf)
		if string(buf[:n]) == msg {
			fmt.Println("SUCCESS!")
			return true
		}
	}
	fmt.Println("FAILED")
	return false
}

func testResolve(hostname string) bool {
	resolver := &net.Resolver{}
	for range 5 {
		ctx := context.Background()
		ips, err := resolver.LookupIPAddr(ctx, hostname)
		if err == nil && len(ips) > 0 {
			fmt.Printf("SUCCESS! Resolved to %s\n", ips[0].IP)
			return true
		}
		time.Sleep(2 * time.Second)
	}
	fmt.Println("FAILED")
	return false
}

func testConnect(host string, port int) bool {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	for range 5 {
		ctx := context.Background()
		conn, err := dialer.DialContext(ctx, "tcp", fmt.Sprintf("%s:%d", host, port))
		if err == nil {
			conn.Close()
			fmt.Println("SUCCESS!")
			return true
		}
		time.Sleep(2 * time.Second)
	}
	fmt.Println("FAILED")
	return false
}
