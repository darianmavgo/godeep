package rtkit

// RTKit wire format. A mailbox message is a 64-bit payload plus an 8-bit
// endpoint. Endpoint 0 is management; types are in payload bits 59:52.
//
// These encodings follow Asahi Linux drivers/soc/apple/rtkit.c. They have NOT
// yet been checked against real hardware from godeep: verify them there
// before the first boot on an M1.
const (
	EPManagement = 0x00
	EPCrashlog   = 0x01
	EPSyslog     = 0x02
	EPDebug      = 0x03
	EPIOReport   = 0x04
	EPOSLog      = 0x08
	EPTraceKit   = 0x0a
	EPAppFirst   = 0x20 // endpoints >= this belong to the coprocessor's driver (SMC, NVMe, ...)

	mgmtHello          = 1
	mgmtHelloReply     = 2
	mgmtStartEP        = 5
	mgmtSetIOPPwrState = 6
	mgmtIOPPwrStateAck = 7
	mgmtEPMap          = 8
	mgmtSetAPPwrState  = 0xb

	bufferRequest = 1 // system endpoints: firmware asks for a shared buffer
	syslogInit    = 8
	syslogLog     = 5

	MinVersion = 11
	MaxVersion = 12
)

// Power states.
const (
	PwrOff      = 0x00
	PwrSleep    = 0x01
	PwrQuiesced = 0x10
	PwrOn       = 0x20
	PwrInit     = 0x220
)

func field(v uint64, hi, lo uint) uint64 { return (v >> lo) & (1<<(hi-lo+1) - 1) }
func put(x uint64, hi, lo uint) uint64   { return (x & (1<<(hi-lo+1) - 1)) << lo }

func msgType(v uint64) uint64  { return field(v, 59, 52) }
func withType(t uint64) uint64 { return put(t, 59, 52) }

// management messages
func helloVersions(v uint64) (lo, hi int) { return int(field(v, 15, 0)), int(field(v, 31, 16)) }
func helloReply(lo, hi int) uint64 {
	return withType(mgmtHelloReply) | put(uint64(lo), 15, 0) | put(uint64(hi), 31, 16)
}
func epmapFields(v uint64) (base int, bitmap uint32, last bool) {
	return int(field(v, 34, 32)), uint32(field(v, 31, 0)), field(v, 51, 51) == 1
}
func epmapReply(base int, more bool) uint64 {
	m := withType(mgmtEPMap) | put(uint64(base), 34, 32) | 1<<51
	if more {
		m |= 1
	}
	return m
}
func startEP(ep uint8) uint64         { return withType(mgmtStartEP) | put(uint64(ep), 39, 32) | 1<<1 }
func pwrState(t uint64, s int) uint64 { return withType(t) | put(uint64(s), 15, 0) }

// buffer requests (crashlog, syslog, ioreport, oslog)
func bufferFields(v uint64) (pages int, iova uint64) {
	return int(field(v, 51, 44)), field(v, 43, 0)
}
func bufferReply(pages int, iova uint64) uint64 {
	return withType(bufferRequest) | put(uint64(pages), 51, 44) | put(iova, 43, 0)
}
