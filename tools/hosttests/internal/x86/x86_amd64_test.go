package x86

import "testing"

func TestCPUIDVendor(t *testing.T) {
	r := CPUIDCount(0, 0)
	vendor := string([]byte{
		byte(r[1]), byte(r[1] >> 8), byte(r[1] >> 16), byte(r[1] >> 24),
		byte(r[3]), byte(r[3] >> 8), byte(r[3] >> 16), byte(r[3] >> 24),
		byte(r[2]), byte(r[2] >> 8), byte(r[2] >> 16), byte(r[2] >> 24),
	})
	if r[0] == 0 || (vendor != "GenuineIntel" && vendor != "AuthenticAMD" && vendor != "HygonGenuine") {
		t.Logf("max leaf %#x, vendor %q", r[0], vendor)
	}
	if r[0] == 0 {
		t.Fatal("CPUID leaf 0 reports no leaves")
	}
}

func TestRDPKRU(t *testing.T) {
	if CPUIDCount(7, 0)[2]&(1<<4) == 0 {
		t.Skip("no OSPKE")
	}
	t.Logf("PKRU %#x", RDPKRU())
}
