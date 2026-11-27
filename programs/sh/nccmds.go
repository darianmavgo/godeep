package sh

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"godeep/kernel/cap"
	"godeep/kernel/proc"
)

func init() {
	builtins["ping"] = builtin{"ping [-c COUNT] [-s SIZE] [-i SECONDS] HOST: ICMP echo requests (4, 56 bytes, 1 s apart); ^C stops", ping}
	builtins["nc"] = builtin{"nc [-u] [-w SECONDS] HOST PORT | nc [-u] -l PORT: a TCP connection (-u: UDP datagrams) between stdin/stdout and HOST:PORT (or the first caller); dialing UDP ends -w seconds (1) after stdin", nc}
}

func ping(ctx context.Context, env Env, p *proc.Proc, args []string) error {
	usage := errors.New("usage: ping [-c COUNT] [-s SIZE] [-i SECONDS] HOST")
	count, size, interval := 4, 56, time.Second
	for len(args) > 1 {
		v, err := strconv.ParseFloat(args[1], 64)
		if err != nil || v < 0 {
			return usage
		}
		switch args[0] {
		case "-c":
			count = int(v)
		case "-s":
			size = int(v)
		case "-i":
			interval = time.Duration(v * float64(time.Second))
		default:
			return usage
		}
		args = args[2:]
	}
	if len(args) != 1 || count < 1 {
		return usage
	}
	host := args[0]
	n, err := network(ctx, env)
	if err != nil {
		return err
	}
	pg, ok := n.(cap.Pinger)
	if !ok {
		return errors.New("this network can't ping")
	}
	var intr <-chan struct{}
	if env.TTY != nil {
		intr = env.TTY.Interrupts()
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stopped := make(chan struct{})
	p.Go(func(context.Context) { // ^C ends the run; quits with ctx
		select {
		case <-intr:
			close(stopped)
			cancel()
		case <-ctx.Done():
		}
	})

	sent, got := 0, 0
	var min, max, sum time.Duration
	for seq := 1; seq <= count; seq++ {
		start := time.Now()
		rctx, rcancel := context.WithTimeout(ctx, max2(interval, 2*time.Second))
		from, rtt, err := pg.Ping(rctx, host, uint16(seq), size)
		rcancel()
		if ctx.Err() != nil {
			break
		}
		sent++
		switch {
		case err != nil && seq == 1 && !errors.Is(err, context.DeadlineExceeded):
			return err // a bad name, no route: say it once
		case err != nil:
			fmt.Fprintf(env.out(), "seq=%d: no reply\n", seq)
		default:
			got++
			if got == 1 || rtt < min {
				min = rtt
			}
			if rtt > max {
				max = rtt
			}
			sum += rtt
			fmt.Fprintf(env.out(), "%d bytes from %s: seq=%d time=%s\n", size+8, from, seq, fmtRTT(rtt))
		}
		if seq < count {
			select {
			case <-time.After(interval - time.Since(start)):
			case <-ctx.Done():
			}
		}
	}
	select {
	case <-stopped:
	default:
		if ctx.Err() != nil && sent == 0 {
			return ctx.Err()
		}
	}
	loss := 100
	if sent > 0 {
		loss = 100 * (sent - got) / sent
	}
	fmt.Fprintf(env.out(), "--- %s: %d sent, %d received, %d%% loss", host, sent, got, loss)
	if got > 0 {
		fmt.Fprintf(env.out(), ", rtt min/avg/max %s/%s/%s", fmtRTT(min), fmtRTT(sum/time.Duration(got)), fmtRTT(max))
	}
	fmt.Fprintln(env.out())
	if got == 0 {
		return statusErr(1)
	}
	return nil
}

func max2(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

func fmtRTT(d time.Duration) string { return fmt.Sprintf("%.3fms", float64(d)/float64(time.Millisecond)) }

// nc connects stdin and stdout to a TCP connection: dialed (nc HOST PORT)
// or the first one accepted (nc -l PORT). At the end of stdin it closes
// its sending half and keeps printing until the other side closes; ^C
// (or a kill) ends it at once. -u: UDP datagrams instead (ncUDP).
func nc(ctx context.Context, env Env, p *proc.Proc, args []string) error {
	usage := errors.New("usage: nc [-u] [-w SECONDS] HOST PORT | nc [-u] -l PORT")
	listen, udp, idle := false, false, time.Second
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		switch args[0] {
		case "-l":
			listen = true
		case "-u":
			udp = true
		case "-w":
			if len(args) < 2 {
				return usage
			}
			f, err := strconv.ParseFloat(args[1], 64)
			if err != nil || f <= 0 {
				return usage
			}
			idle = time.Duration(f * float64(time.Second))
			args = args[1:]
		default:
			return usage
		}
		args = args[1:]
	}
	var addr string
	switch {
	case listen && len(args) == 1:
		addr = net.JoinHostPort("", args[0])
	case !listen && len(args) == 2:
		addr = net.JoinHostPort(args[0], args[1])
	case !listen && len(args) == 1:
		addr = args[0] // HOST:PORT
	default:
		return usage
	}
	n, err := network(ctx, env)
	if err != nil {
		return err
	}
	if udp {
		return ncUDP(ctx, env, p, n, listen, addr, idle)
	}
	var conn net.Conn
	if listen {
		l, err := n.Listen(ctx, "tcp", addr)
		if err != nil {
			return err
		}
		conn, err = acceptOne(ctx, env, p, l)
		l.Close()
		if err != nil {
			return err
		}
	} else if conn, err = n.Dial(ctx, "tcp", addr); err != nil {
		return err
	}
	defer conn.Close()

	in, release := env.input(ctx)
	defer release()
	recvd := make(chan error, 1)
	if !p.Go(func(context.Context) {
		_, err := io.Copy(env.out(), conn)
		recvd <- err
	}) {
		return proc.ErrKilled
	}
	// The sender ends when its input does: the terminal reader is
	// released below, a pipe closes when this stage ends.
	p.Go(func(context.Context) {
		io.Copy(conn, in)
		if cw, ok := conn.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		}
	})
	var intr <-chan struct{}
	if env.TTY != nil {
		intr = env.TTY.Interrupts()
	}
	select {
	case err := <-recvd:
		release() // stop reading the terminal
		conn.Close()
		if errors.Is(err, net.ErrClosed) {
			return nil
		}
		return err
	case <-intr:
	case <-ctx.Done():
	}
	release()
	conn.Close()
	<-recvd
	return ctx.Err()
}

// acceptOne waits for one connection; ^C or ctx stops the wait.
func acceptOne(ctx context.Context, env Env, p *proc.Proc, l net.Listener) (net.Conn, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if env.TTY != nil {
		intr := env.TTY.Interrupts()
		p.Go(func(context.Context) {
			select {
			case <-intr:
				l.Close()
			case <-ctx.Done():
			}
		})
	}
	stop := context.AfterFunc(ctx, func() { l.Close() })
	defer stop()
	c, err := l.Accept()
	if err != nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return c, err
}

// ncUDP: each read from stdin (a line from the terminal) is one datagram,
// each datagram received is printed. Dialing, it ends once stdin has ended
// and nothing arrived for idle; listening (-l), it answers the last sender
// and runs until ^C or a kill.
func ncUDP(ctx context.Context, env Env, p *proc.Proc, n cap.Net, listen bool, addr string, idle time.Duration) error {
	var pc net.PacketConn
	var peer net.Addr
	var send func([]byte) error // dialing: the connected socket's Write
	var mu sync.Mutex           // peer
	if listen {
		c, err := cap.ListenPacket(ctx, n, "udp", addr)
		if err != nil {
			return err
		}
		pc = c
	} else {
		c, err := n.Dial(ctx, "udp", addr)
		if err != nil {
			return err
		}
		upc, ok := c.(net.PacketConn)
		if !ok {
			c.Close()
			return errors.New("this network's UDP sockets can't receive")
		}
		pc = upc
		send = func(b []byte) error { _, err := c.Write(b); return err }
	}
	defer pc.Close()
	in, release := env.input(ctx)
	defer release()
	inDone := make(chan struct{})
	p.Go(func(context.Context) {
		defer close(inDone)
		buf := make([]byte, 65000)
		for {
			k, err := in.Read(buf)
			if k > 0 {
				mu.Lock()
				to := peer
				mu.Unlock()
				var werr error
				switch {
				case send != nil:
					werr = send(buf[:k])
				case to != nil:
					_, werr = pc.WriteTo(buf[:k], to)
				}
				if werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	})
	got := make(chan error)
	stop := make(chan struct{})
	defer close(stop) // before pc.Close: the reader then can't block on got
	p.Go(func(context.Context) {
		buf := make([]byte, 65536)
		for {
			k, from, err := pc.ReadFrom(buf)
			if err != nil {
				select {
				case got <- err:
				case <-stop:
				}
				return
			}
			if listen {
				mu.Lock()
				peer = from
				mu.Unlock()
			}
			env.out().Write(buf[:k])
			select {
			case got <- nil:
			case <-stop:
				return
			}
		}
	})
	var intr <-chan struct{}
	if env.TTY != nil {
		intr = env.TTY.Interrupts()
	}
	var quiet <-chan time.Time // after stdin's end (dialing): the idle timer
	inEnd := (<-chan struct{})(inDone)
	for {
		select {
		case <-inEnd:
			inEnd = nil
			if !listen {
				quiet = time.After(idle)
			}
		case err := <-got:
			if err != nil {
				return nil
			}
			if quiet != nil {
				quiet = time.After(idle)
			}
		case <-quiet:
			return nil
		case <-intr:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
