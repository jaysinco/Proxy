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
		fmt.Printf("unsupport protocol: '%s'\n", os.Args[1])
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
	info <- clientConnect
	defer func() {
		if !closed {
			conn.Close()
			info <- clientClose
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
	remote, err := net.DialTimeout("tcp", remoteAddr, 20*time.Second)
	if err != nil {
		fmt.Fprint(conn, "HTTP/1.1 404 Failed to connect host\r\n\r\n")
		return
	}
	info <- remoteConnect
	defer func() {
		if !closed {
			remote.Close()
			info <- remoteClose
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
	signal := make(chan struct{})
	go ncopy(remote, conn, signal)
	go ncopy(conn, remote, signal)
	<-signal
	conn.Close()
	remote.Close()
	<-signal
	info <- clientClose
	info <- remoteClose
	closed = true
}

var pool = &leakyBuf{4096, make(chan []byte, 2048)}

func ncopy(dst, src net.Conn, signal chan struct{}) {
	defer func() {
		signal <- struct{}{}
	}()
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

var info = make(chan cmsg, 3)

type cmsg int

const (
	clientConnect cmsg = iota
	clientClose
	remoteConnect
	remoteClose
)

func collect() {
	var ccon, rcon int
	for msg := range info {
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
