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
