package main

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	if len(os.Args[1:]) != 2 {
		fmt.Println("Usage: proxy [protocol] [ip:port]")
		return
	}
	protocol := map[string]func(net.Conn){
		"http": handleHTTP,
	}
	handle, ok := protocol[os.Args[1]]
	if !ok {
		fmt.Printf("unsupport proxy type: %s\n", os.Args[1])
		return
	}
	listener, err := net.Listen("tcp", os.Args[2])
	if err != nil {
		fmt.Printf("tcp listen: %v\n", err)
		return
	}
	fmt.Printf("listening on %s...", os.Args[2])
	go collect()
	for {
		if conn, err := listener.Accept(); err == nil {
			go handle(conn)
		}
	}
}

func handleHTTP(conn net.Conn) {
	closed := false
	sysinfo <- clientConnect
	defer func() {
		if !closed {
			conn.Close()
			sysinfo <- clientClose
		}
	}()
	req, err := http.ReadRequest(bufio.NewReader(conn))
	if err != nil {
		fmt.Fprint(conn, "HTTP/1.1 400 Not a http request\r\n\r\n")
		return
	}
	remoteAddr := req.Host
	if ok := strings.Contains(remoteAddr, ":"); !ok {
		remoteAddr += ":80"
	}
	if conn.LocalAddr().String() == remoteAddr {
		fmt.Fprint(conn, "HTTP/1.1 403 Host address looped\r\n\r\n")
		return
	}
	remote, err := net.Dial("tcp", remoteAddr)
	if err != nil {
		fmt.Fprint(conn, "HTTP/1.1 404 Failed to connect host\r\n\r\n")
		return
	}
	sysinfo <- remoteConnect
	defer func() {
		if !closed {
			remote.Close()
			sysinfo <- remoteClose
		}
	}()
	switch req.Method {
	case "CONNECT":
		if _, err := fmt.Fprint(conn, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
			return
		}
	default:
		if err := req.Write(remote); err != nil {
			return
		}
	}
	go func() {
		copyThenClose(remote, conn)
		sysinfo <- remoteClose
	}()
	copyThenClose(conn, remote)
	sysinfo <- clientClose
	closed = true
}

var pool = &leakyBuf{4096, make(chan []byte, 2048)}

func copyThenClose(dst, src net.Conn) {
	defer dst.Close()
	buf := pool.Get()
	defer pool.Put(buf)
	for {
		src.SetReadDeadline(time.Now().Add(20 * time.Second))
		n, err := src.Read(buf)
		if n > 0 {
			if _, err := dst.Write(buf[0:n]); err != nil {
				break
			}
		}
		if err != nil {
			break
		}
	}
}

type leakyBuf struct {
	size int
	free chan []byte
}

func (l *leakyBuf) Get() (b []byte) {
	select {
	case b = <-l.free:
	default:
		b = make([]byte, l.size)
	}
	return
}

func (l *leakyBuf) Put(b []byte) {
	select {
	case l.free <- b:
	default:
	}
	return
}

var sysinfo = make(chan sysmsg, 3)

type sysmsg int

const (
	clientConnect sysmsg = iota
	clientClose
	remoteConnect
	remoteClose
)

func collect() {
	var ccon, rcon int
	for msg := range sysinfo {
		switch msg {
		case clientConnect:
			ccon++
		case clientClose:
			ccon--
		case remoteConnect:
			rcon++
		case remoteClose:
			rcon--
		}
		fmt.Printf("\r%s://%s/info?client=%d&remote=%d&leakybuf=%d/", os.Args[1], os.Args[2],
			ccon, rcon, len(pool.free))
	}
}
