package mmio

import (
	"testing"

	kmmio "godeep/kernel/mmio"
)

func TestDescribe(t *testing.T) {
	f := kmmio.NewFake()
	if Present(f) || Describe(f) != "not a virtio-mmio device" {
		t.Fatal("blank window")
	}
	f.Set(MagicValue, Magic)
	f.Set(Version, 2)
	if Present(f) || Describe(f) != "empty slot" {
		t.Fatal("empty slot")
	}
	f.Set(DeviceID, 4)
	f.Set(VendorID, 0x554d4551)
	if !Present(f) || Describe(f) != "rng (transport v2, vendor 0x554d4551)" {
		t.Fatalf("rng: %q", Describe(f))
	}
}
