package libvirtsrc

import (
	"os"
	"testing"
	"time"

	"pademelon/internal/model"
)

// loadFixture parses one of the testdata domain XMLs. Both fixtures are
// sanitized copies of real dumpxml output from a TrueNAS box — the shapes
// libvirt actually hands us, not idealised ones.
func loadFixture(t *testing.T, name string) domainXML {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	return parseDomainXML(string(raw))
}

func TestParseDomainXMLAlpine(t *testing.T) {
	dx := loadFixture(t, "alpine.xml")
	if dx.UUID != "6a1b0c9e-11d2-4f8a-9b3c-7e5d0a1f2b3c" {
		t.Errorf("uuid = %q", dx.UUID)
	}
	if got := agentChannelState(&dx); got != "connected" {
		t.Errorf("agent channel state = %q, want connected", got)
	}

	disks := diskShapes(&dx)
	if len(disks) != 1 {
		t.Fatalf("want 1 real disk, got %d", len(disks))
	}
	d := disks[0]
	if d.Dev != "vda" || d.Format != "raw" || d.Bus != "virtio" {
		t.Errorf("disk = %+v, want vda/raw/virtio", d)
	}
	if d.Source != "/dev/zvol/nvme/vms/alpine_test-bxuwle" {
		t.Errorf("disk source = %q", d.Source)
	}

	nics := nicShapes(&dx)
	if len(nics) != 1 {
		t.Fatalf("want 1 nic, got %d", len(nics))
	}
	if nics[0].Device != "vnet5" || nics[0].Bridge != "br1" {
		t.Errorf("nic = %+v, want vnet5 on br1", nics[0])
	}
	if nics[0].MAC != "52:54:00:aa:bb:01" {
		t.Errorf("mac = %q", nics[0].MAC)
	}
}

func TestParseDomainXMLWindowsFiltersCdroms(t *testing.T) {
	dx := loadFixture(t, "windows.xml")
	disks := diskShapes(&dx)

	// Two ISO cdroms plus one zvol: only the zvol is storage. The cdroms
	// would otherwise show as mystery disks nobody can mount.
	if len(disks) != 1 || disks[0].Dev != "vda" {
		t.Fatalf("disks = %+v, want only the vda zvol", disks)
	}
	if disks[0].Source != "/dev/zvol/nvme/vms/wintest-hnlane" {
		t.Errorf("disk source = %q", disks[0].Source)
	}

	nics := nicShapes(&dx)
	if len(nics) != 1 {
		t.Fatalf("want 1 nic, got %d", len(nics))
	}
	if nics[0].Device != "vnet7" {
		t.Errorf("nic device = %q, want vnet7", nics[0].Device)
	}

	// A stopped (or freshly defined) domain's channel carries no state
	// attribute, so this parses as empty. inspect only consults the state
	// for running VMs, where libvirt always fills it in.
	if got := agentChannelState(&dx); got != "" {
		t.Errorf("channel state = %q, want empty", got)
	}
}

func TestBytesPerSecond(t *testing.T) {
	sec := time.Second

	if got, ok := bytesPerSecond(1000, 3000, sec); got != 2000 || !ok {
		t.Errorf("normal case = (%d, %v), want (2000, true)", got, ok)
	}

	// Counter went backwards: the VM restarted, and the old sample belongs
	// to a previous lifetime.
	if _, ok := bytesPerSecond(3000, 1000, sec); ok {
		t.Error("backwards counter should report not-ok")
	}

	if _, ok := bytesPerSecond(1000, 3000, 0); ok {
		t.Error("zero elapsed should report not-ok")
	}
}

func TestJoinNicGuestNames(t *testing.T) {
	vm := &model.VM{
		Nics: []model.Nic{
			{Device: "vnet5", MAC: "52:54:00:aa:bb:01"},
			{Device: "vnet6", MAC: "52:54:00:aa:bb:09"},
		},
		Interfaces: []model.Iface{
			{Name: "eth0", MAC: "52:54:00:AA:BB:01"}, // case differs on purpose
		},
	}
	joinNicGuestNames(vm)

	if vm.Nics[0].GuestName != "eth0" {
		t.Errorf("guest name = %q, want eth0 (MAC match must ignore case)", vm.Nics[0].GuestName)
	}
	if vm.Nics[1].GuestName != "" {
		t.Errorf("unmatched nic guest name = %q, want empty", vm.Nics[1].GuestName)
	}
}
