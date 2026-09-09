package tests

import (
	"encoding/binary"
	"fmt"
	"os"

	"github.com/aledbf/pvm-testbed/initrd/internal/harness"
)

// Offsets into struct boot_params, from Documentation/arch/x86/boot.rst.
const (
	bpVersion      = 0x206 // setup_header.version, 2 bytes
	bpTypeOfLoader = 0x210 // setup_header.type_of_loader, 1 byte
	bpLoadflags    = 0x211
	kaslrFlag      = 0x02

	// init_pvh_bootparams() stamps exactly this: "Version 2.12 supports
	// the Xen entry point".  QEMU's own bzImage loader copies whatever
	// the image's header says instead, which for a v7.3 kernel is later
	// than 2.12, so the two paths are distinguishable.
	pvhBootProtoVersion = 0x020c
)

func registerEntryPath(h *harness.Harness) {
	// Which entry point ran is not cosmetic here: a PVM guest is entered
	// through the PVH ELF note, so a run that quietly came up the
	// bzImage way has not tested the path that matters.
	h.Add(harness.Case{
		Name:   "boot/entry-path",
		Suites: []string{harness.Smoke},
		Fn: func(t *harness.T) error {
			b, err := os.ReadFile("/sys/kernel/boot_params/data")
			if err != nil {
				return fmt.Errorf("read boot_params (CONFIG_DEBUG_BOOT_PARAMS?): %w", err)
			}
			if len(b) < bpLoadflags+1 {
				return fmt.Errorf("boot_params is %d bytes, too short", len(b))
			}
			version := binary.LittleEndian.Uint16(b[bpVersion:])
			loader := b[bpTypeOfLoader]
			flags := b[bpLoadflags]

			path := "bzImage or direct"
			if version == pvhBootProtoVersion {
				path = "PVH entry (pvh_start_xen)"
			}
			t.Logf("boot protocol 0x%04x, type_of_loader 0x%02x, loadflags 0x%02x",
				version, loader, flags)
			t.Logf("entry path: %s", path)
			t.Logf("KASLR flag: %v", flags&kaslrFlag != 0)

			// When the harness says it booted the ELF image, the PVH
			// note is the only way in, so disagreement is a real result.
			want := cmdlineTag()
			if want == "pvh" && version != pvhBootProtoVersion {
				return fmt.Errorf("booted for the PVH path but the boot protocol is "+
					"0x%04x, not 0x%04x: qemu did not use the ELF note",
					version, pvhBootProtoVersion)
			}
			return nil
		},
	})
}
