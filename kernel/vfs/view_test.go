package vfs_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"godeep/fs/memfs"
	"godeep/kernel/vfs"
)

func TestView(t *testing.T) {
	ctx := context.Background()
	ns := vfs.New(ctx, nil)
	ns.Mount("ram", memfs.New("ram"))
	ns.Mount("tmp", memfs.New("tmp"))

	v := ns.View([]string{"ram"}, true)
	if got := v.Names(ctx); !slices.Equal(got, []string{"ram"}) {
		t.Fatalf("view names %v", got)
	}
	if _, err := v.Open(ctx, "tmp"); err == nil {
		t.Fatal("view opened tmp")
	}
	r, err := v.Open(ctx, "ram")
	if err != nil {
		t.Fatal(err)
	}
	if vfs.IsWritable(r) {
		t.Fatal("read-only view gave a writable volume")
	}
	if v.CanMount() {
		t.Fatal("view can mount")
	}
	v.Mount("evil", memfs.New("evil"))
	if _, err := ns.Open(ctx, "evil"); err == nil {
		t.Fatal("view mounted into the namespace")
	}
	if err := v.Unmount("ram"); !errors.Is(err, vfs.ErrPermission) {
		t.Fatalf("view unmount: %v", err)
	}

	// Narrowing a writable view of everything: still writable, still
	// limited by the view below.
	w := ns.View(nil, false)
	if r, _ := w.Open(ctx, "tmp"); !vfs.IsWritable(r) {
		t.Fatal("writable view lost writes")
	}
	if _, err := v.View([]string{"ram", "tmp"}, false).Open(ctx, "tmp"); err == nil {
		t.Fatal("nested view widened")
	}
	if r, _ := v.View(nil, false).Open(ctx, "ram"); vfs.IsWritable(r) {
		t.Fatal("nested view made ram writable")
	}
	if v.Context() != ctx {
		t.Fatal("view context")
	}
}
