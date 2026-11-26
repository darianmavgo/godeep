package rtkit

import (
	"context"
	"strings"
	"testing"
	"time"

	"godeep/kernel/dma"
)

// pipe is an in-memory mailbox; iop is the firmware's end of it.
type pipe struct{ toIOP, toAP chan Message }

func (p *pipe) Send(ctx context.Context, m Message) error {
	select {
	case p.toIOP <- m:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (p *pipe) Recv() <-chan Message { return p.toAP }

const smcEP = 0x20

// fakeIOP plays the firmware side of the protocol.
func fakeIOP(t *testing.T, p *pipe, crash <-chan struct{}) {
	expect := func(ep uint8, typ uint64) uint64 {
		t.Helper()
		select {
		case m := <-p.toIOP:
			if m.EP != ep || msgType(m.Data) != typ {
				t.Errorf("iop: got ep %#x type %d, want ep %#x type %d", m.EP, msgType(m.Data), ep, typ)
			}
			return m.Data
		case <-time.After(2 * time.Second):
			t.Errorf("iop: timed out waiting for ep %#x type %d", ep, typ)
			return 0
		}
	}
	send := func(ep uint8, v uint64) { p.toAP <- Message{EP: ep, Data: v} }

	send(EPManagement, withType(mgmtHello)|put(11, 15, 0)|put(12, 31, 16))
	if _, max := helloVersions(expect(EPManagement, mgmtHelloReply)); max != 12 {
		t.Errorf("negotiated version %d", max)
	}
	// endpoints: crashlog, syslog, ioreport, SMC (0x20)
	send(EPManagement, withType(mgmtEPMap)|put(0, 34, 32)|1<<EPCrashlog|1<<EPSyslog|1<<EPIOReport) // not last
	if reply := expect(EPManagement, mgmtEPMap); reply&1 == 0 {
		t.Errorf("AP did not ask for more endpoints after a non-final page")
	}
	send(EPManagement, withType(mgmtEPMap)|put(1, 34, 32)|1<<0|1<<51) // base 1, bit 0 = ep 0x20, LAST
	if reply := expect(EPManagement, mgmtEPMap); reply&1 != 0 {
		t.Errorf("AP asked for more endpoints after the LAST page")
	}
	for range []uint8{EPCrashlog, EPSyslog, EPIOReport} {
		expect(EPManagement, mgmtStartEP)
	}
	// booted: the IOP reports itself on; syslog asks for a 2-page buffer;
	// meanwhile the AP sets its power state on. Either order.
	send(EPManagement, pwrState(mgmtIOPPwrStateAck, PwrOn))
	send(EPSyslog, withType(bufferRequest)|put(2, 51, 44))
	for i := 0; i < 2; i++ {
		var m Message
		select {
		case m = <-p.toIOP:
		case <-time.After(2 * time.Second):
			t.Error("iop: timed out waiting for buffer reply / power request")
			return
		}
		switch {
		case m.EP == EPSyslog && msgType(m.Data) == bufferRequest:
			if pages, iova := bufferFields(m.Data); pages != 2 || iova == 0 || iova%16384 != 0 {
				t.Errorf("bad buffer reply: pages %d iova %#x", pages, iova)
			}
		case m.EP == EPManagement && msgType(m.Data) == mgmtSetAPPwrState:
			send(EPManagement, pwrState(mgmtSetAPPwrState, int(field(m.Data, 15, 0))))
		default:
			t.Errorf("iop: unexpected ep %#x type %d", m.EP, msgType(m.Data))
		}
	}

	// SMC endpoint: echo requests +1
	expect(EPManagement, mgmtStartEP)
	for i := 0; i < 3; i++ {
		v := expect(smcEP, msgType(0)) // app messages have no RTKit type
		send(smcEP, v+1)
	}
	if crash != nil {
		<-crash
		send(EPCrashlog, withType(bufferRequest)|put(1, 51, 44)) // first: buffer request
		expect(EPCrashlog, bufferRequest)
		send(EPCrashlog, withType(bufferRequest)|put(1, 51, 44)|put(0x1000, 43, 0)) // again: crashed
	}
}

func TestBootAndEndpoint(t *testing.T) {
	for _, crash := range []bool{false, true} {
		p := &pipe{toIOP: make(chan Message, 8), toAP: make(chan Message, 8)}
		alloc := dma.New(dma.Region{Base: 0x1_2000_0000, Mem: make([]byte, 1<<20)})
		r := New("smc", p, alloc, dma.Identity{})
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		runErr := make(chan error, 1)
		go func() { runErr <- r.Run(ctx) }()
		var crashNow chan struct{}
		if crash {
			crashNow = make(chan struct{})
		}
		go fakeIOP(t, p, crashNow)

		if err := r.Boot(ctx); err != nil {
			t.Fatal(err)
		}
		recv, send, err := r.Endpoint(ctx, smcEP)
		if err != nil {
			t.Fatal(err)
		}
		for i := uint64(0); i < 3; i++ {
			if err := send(ctx, 100+i); err != nil {
				t.Fatal(err)
			}
			if got := <-recv; got != 101+i {
				t.Fatalf("echo: got %d", got)
			}
		}
		in := r.Info()
		if in.Version != 12 || len(in.Endpoints) != 4 || in.Buffers[EPSyslog] != 2*4096 {
			t.Fatalf("info: %+v", in)
		}
		if _, _, err := r.Endpoint(ctx, 0x33); err == nil {
			t.Fatal("started an endpoint the firmware doesn't have")
		}

		if crash {
			close(crashNow)
			if err := <-runErr; err == nil || !strings.Contains(err.Error(), "crashed") {
				t.Fatalf("want crash error, got %v", err)
			}
		} else {
			cancel()
			<-runErr
		}
		cancel()
		if s := alloc.Stats(); s.Used != 0 {
			t.Fatalf("shared buffers leaked: %+v", s)
		}
	}
}

func TestRejectsOldFirmware(t *testing.T) {
	p := &pipe{toIOP: make(chan Message, 8), toAP: make(chan Message, 8)}
	r := New("old", p, nil, nil)
	p.toAP <- Message{EP: EPManagement, Data: withType(mgmtHello) | put(9, 15, 0) | put(10, 31, 16)}
	if err := r.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("got %v", err)
	}
}
