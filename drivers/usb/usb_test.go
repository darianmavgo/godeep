package usb

import "testing"

func TestDescriptors(t *testing.T) {
	d, err := ParseDevice([]byte{18, DescDevice, 0x10, 0x03, 0, 0, 0, 9, 0x5e, 0x04, 0x34, 0x12, 0, 0, 1, 2, 3, 1})
	if err != nil || d.MaxPacket0 != 512 || d.Vendor != 0x045e || d.Product != 0x1234 || d.Configs != 1 {
		t.Fatalf("SuperSpeed device: %+v %v", d, err)
	}
	if _, err := ParseDevice([]byte{9, DescConfig, 0, 0, 0, 0, 0, 0}); err == nil {
		t.Fatal("config accepted as device")
	}
	c, err := ParseConfig([]byte{
		9, DescConfig, 41, 0, 2, 1, 0, 0xa0, 50,
		9, DescInterface, 0, 0, 1, ClassHID, SubclassBoot, ProtoKeyboard, 0,
		7, DescEndpoint, 0x81, 3, 8, 0, 10,
		9, DescInterface, 1, 0, 1, ClassHID, 0, 0, 0,
		7, DescEndpoint, 0x82, 3, 64, 0, 1,
	})
	if err != nil || len(c.Interfaces) != 2 || c.Interfaces[0].BootKind() != "keyboard" || c.Interfaces[1].BootKind() != "" {
		t.Fatalf("config %+v %v", c, err)
	}
	if e := c.Interfaces[1].Endpoints[0]; !e.IsInterruptIn() || e.Num() != 2 || e.MaxPacket != 64 {
		t.Fatalf("endpoint %+v", e)
	}
	if _, err := ParseConfig([]byte{9, DescConfig, 20, 0, 1, 1, 0, 0, 0, 30, DescInterface}); err == nil {
		t.Fatal("truncated config accepted")
	}
}

func TestUASPipes(t *testing.T) {
	// A USB-C SSD's interface 0: alt 0 Bulk-Only, alt 1 UAS with pipe
	// usage descriptors (on SuperSpeed, each after an endpoint companion).
	ep := func(addr byte, pipe byte) []byte {
		return []byte{7, DescEndpoint, addr, 2, 0, 2, 0, 6, descSSCompanion, 0, 0, 0, 0, 4, DescCSInterface, pipe, 0}
	}
	b := []byte{9, DescConfig, 0, 0, 1, 1, 0, 0x80, 50,
		9, DescInterface, 0, 0, 2, ClassMassStorage, 6, ProtoBOT, 0,
		7, DescEndpoint, 0x81, 2, 0, 2, 0, 7, DescEndpoint, 0x02, 2, 0, 2, 0,
		9, DescInterface, 0, 1, 4, ClassMassStorage, 6, ProtoUAS, 0}
	b = append(b, ep(0x01, PipeCommand)...)
	b = append(b, ep(0x82, PipeStatus)...)
	b = append(b, ep(0x83, PipeDataIn)...)
	b = append(b, ep(0x04, PipeDataOut)...)
	b[2] = byte(len(b))
	c, err := ParseConfig(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Interfaces) != 2 || c.Interfaces[1].Protocol != ProtoUAS || c.Interfaces[1].Alt != 1 {
		t.Fatalf("interfaces %+v", c.Interfaces)
	}
	for i, want := range []byte{PipeCommand, PipeStatus, PipeDataIn, PipeDataOut} {
		if got := c.Interfaces[1].Endpoints[i].PipeID; got != want {
			t.Errorf("endpoint %d pipe %d, want %d", i, got, want)
		}
	}
	if len(c.Interfaces[1].Extra) != 0 || c.Interfaces[0].Endpoints[0].PipeID != 0 {
		t.Errorf("pipe descriptors leaked: %+v", c.Interfaces)
	}
}
