// godeep patch: keep the MMU state outside init.go, so the linkhwinit0 build
// tag can exclude Init (as on amd64) while CPU.Init still links.

package arm64

var m mmu
