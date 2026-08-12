package main

import (
	"bufio"
	"net"
	"sync"
	"time"
)

// clientHandshakeTimeout 是"客户端连上 → 说完开场白"的上限：嗅探首字节用它，SOCKS5 的
// 方法协商 + 请求也用它。没有这个上限，一条连上却不发字节的连接会永久占住一个
// goroutine 和一个 fd。
//
// 用完必须清掉 deadline，否则整条隧道会在到点时被掐断（dialer.go 的 connectHandshake
// 里是同一个坑）。
const clientHandshakeTimeout = 10 * time.Second

// sniffListener 在同一个端口上分流 HTTP 与 SOCKS5：SOCKS5 连接就地交给 socks5 消化，
// 其余的经 conns 递给 http.Server —— 对 http.Server 而言它就是个普通 net.Listener。
//
// 判别只看第一个字节：SOCKS5 客户端开口必是 0x05（RFC 1928 的 VER），而 HTTP 代理请求
// 的第一个字节必是方法名首字母（G/P/C/H/D/O/T），全是 ASCII 大写字母，两者不相交。
// SOCKS4（0x04）不单独处理：它会落进 HTTP 分支，由 net/http 回 400 关掉，为它写一条
// 专门的拒绝逻辑是给不存在的用户写代码。
type sniffListener struct {
	net.Listener
	socks5 func(net.Conn)
	conns  chan net.Conn

	closeOnce sync.Once
	done      chan struct{}
}

func newSniffListener(inner net.Listener, socks5 func(net.Conn)) *sniffListener {
	l := &sniffListener{
		Listener: inner,
		socks5:   socks5,
		conns:    make(chan net.Conn),
		done:     make(chan struct{}),
	}
	go l.loop()
	return l
}

// loop 只负责 accept，判别一律甩给每连接的 goroutine。
//
// ★ peek 绝不能挪进 Accept() 或这个循环里。它是阻塞 I/O：一条连上却不发字节的连接
// 会卡住整个 accept 循环，后面所有新连接排队等它超时——一行位置之差就是拒绝服务。
func (l *sniffListener) loop() {
	defer l.markDone()
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return // listener 已关闭
		}
		go l.dispatch(c)
	}
}

func (l *sniffListener) dispatch(c net.Conn) {
	c.SetReadDeadline(time.Now().Add(clientHandshakeTimeout))
	br := bufio.NewReader(c)
	first, err := br.Peek(1)
	if err != nil {
		c.Close()
		return
	}
	c.SetReadDeadline(time.Time{})

	conn := &bufferedConn{Conn: c, r: br}
	if first[0] == socks5Version {
		l.socks5(conn)
		return
	}
	select {
	case l.conns <- conn:
	case <-l.done:
		conn.Close()
	}
}

func (l *sniffListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *sniffListener) Close() error {
	l.markDone()
	return l.Listener.Close()
}

// markDone 关的是 done 而不是 conns：dispatch 的 goroutine 可能正阻塞在往 conns 发送，
// 关 conns 会让它 panic。
func (l *sniffListener) markDone() {
	l.closeOnce.Do(func() { close(l.done) })
}

// bufferedConn 把已经被读进 bufio 的字节还回读取路径，底层仍是原连接，
// Close / SetDeadline / 地址等语义不变。两处都要它：
//
//   - 嗅探时预读的那一个字节；
//   - CONNECT 路径上 Hijack 交还的 bufio —— 客户端若把 CONNECT 与紧随其后的
//     TLS ClientHello 塞进同一个 TCP 段，那几个字节就躺在里面，丢掉它等于隧道建好
//     却永远握不上手。
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }
