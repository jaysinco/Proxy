package main

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

func main() {
	if len(os.Args[1:]) != 2 {
		fmt.Println("Usage: proxy [protocol] [ip:port]")
		return
	}
	protocol := map[string]func(net.Conn){
		"http":   handleHTTP,
		"socks5": handleSocks5,
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
	fmt.Printf("listening on %s://%s...\n", os.Args[1], os.Args[2])
	go countTCP()
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
	done := make(chan struct{})
	go ncopy(remote, conn, done)
	go ncopy(conn, remote, done)
	<-done
	conn.Close()
	remote.Close()
	<-done
	info <- clientClose
	info <- remoteClose
	closed = true
}

func handleSocks5(conn net.Conn) {
	closed := false
	info <- clientConnect
	defer func() {
		if !closed {
			conn.Close()
			info <- clientClose
		}
	}()
	if err := handshakeSs5(conn); err != nil {
		return
	}
	remoteAddr, err := remoteAddrSs5(conn)
	if err != nil {
		return
	}
	if _, err := conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x68, 0x68}); err != nil {
		return
	}
	remote, err := net.DialTimeout("tcp", remoteAddr, 20*time.Second)
	if err != nil {
		return
	}
	info <- remoteConnect
	defer func() {
		if !closed {
			remote.Close()
			info <- remoteClose
		}
	}()
	done := make(chan struct{})
	go ncopy(remote, conn, done)
	go ncopy(conn, remote, done)
	<-done
	conn.Close()
	remote.Close()
	<-done
	info <- clientClose
	info <- remoteClose
	closed = true
}

func handshakeSs5(conn net.Conn) error {
	buf := make([]byte, 257)
	n, err := io.ReadAtLeast(conn, buf, 2)
	if err != nil {
		return fmt.Errorf("failed to read first two bytes")
	}
	if buf[0] != 0x05 {
		return fmt.Errorf("wrong socks verion")
	}
	nmethods := int(buf[1])
	mlen := nmethods + 2
	switch {
	case n > mlen:
		return fmt.Errorf("more bytes than expected")
	case n < mlen:
		if _, err = io.ReadFull(conn, buf[n:mlen]); err != nil {
			return fmt.Errorf("read rest: %v", err)
		}
	}
	if _, err := conn.Write([]byte{0x05, 0x00}); err != nil {
		return fmt.Errorf("reply: %v", err)
	}
	return nil
}

func remoteAddrSs5(conn net.Conn) (string, error) {
	buf := make([]byte, 262)
	n, err := io.ReadAtLeast(conn, buf, 5)
	if err != nil {
		return "", fmt.Errorf("failed to read five two bytes")
	}
	if buf[0] != 0x05 {
		return "", fmt.Errorf("wrong socks verion")
	}
	if buf[1] != 0x01 {
		return "", fmt.Errorf("only support command 'connect'")
	}
	mlen := -1
	switch buf[3] {
	case 0x01:
		mlen = 4 + net.IPv4len + 2
	case 0x03:
		mlen = 4 + 1 + int(buf[4]) + 2
	case 0x04:
		mlen = 4 + net.IPv6len + 2
	default:
		return "", fmt.Errorf("wrong remote address type")
	}
	switch {
	case n > mlen:
		return "", fmt.Errorf("more bytes than expected")
	case n < mlen:
		if _, err = io.ReadFull(conn, buf[n:mlen]); err != nil {
			return "", fmt.Errorf("read rest: %v", err)
		}
	}
	host := ""
	switch buf[3] {
	case 0x01:
		host = net.IP(buf[4 : 4+net.IPv4len]).String()
	case 0x03:
		host = string(buf[5 : 5+int(buf[4])])
	case 0x04:
		host = net.IP(buf[4 : 4+net.IPv6len]).String()
	}
	port := binary.BigEndian.Uint16(buf[mlen-2 : mlen])
	host = net.JoinHostPort(host, strconv.Itoa(int(port)))
	return host, nil
}

var pool = &leakyBuf{4096, make(chan []byte, 2048)}

func ncopy(dst, src net.Conn, done chan struct{}) {
	defer func() {
		done <- struct{}{}
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

func countTCP() {
	fmt.Printf("current connected TCP: %-6d", 0)
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
		fmt.Printf("\rcurrent connected TCP: %-6d", ccon+rcon)
	}
}
