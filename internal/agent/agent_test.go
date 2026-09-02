package agent

import (
	"testing"

	"pademelon/internal/model"
)

// realInterfaceReply is an actual guest-network-get-interfaces reply from an
// Ubuntu 24.04 VM running Docker. It's the reason interface filtering exists:
// seven interfaces, and exactly one of them is the answer to "what IP is this
// box on".
const realInterfaceReply = `{"return":[
{"name":"lo","ip-addresses":[{"ip-address-type":"ipv4","ip-address":"127.0.0.1","prefix":8},{"ip-address-type":"ipv6","ip-address":"::1","prefix":128}],"hardware-address":"00:00:00:00:00:00"},
{"name":"ens3","ip-addresses":[{"ip-address-type":"ipv4","ip-address":"192.168.1.13","prefix":24},{"ip-address-type":"ipv6","ip-address":"fe80::2a0:98ff:fe63:1795","prefix":64}],"hardware-address":"00:a0:98:63:17:95"},
{"name":"docker0","ip-addresses":[{"ip-address-type":"ipv4","ip-address":"172.17.0.1","prefix":16},{"ip-address-type":"ipv6","ip-address":"fe80::f090:65ff:fe9a:2610","prefix":64}],"hardware-address":"f2:90:65:9a:26:10"},
{"name":"br-8f456164fabf","ip-addresses":[{"ip-address-type":"ipv4","ip-address":"172.18.0.1","prefix":16}],"hardware-address":"da:d0:29:b9:e1:65"},
{"name":"vethb451210","ip-addresses":[{"ip-address-type":"ipv6","ip-address":"fe80::38c9:3fff:fe8c:b1f6","prefix":64}],"hardware-address":"3a:c9:3f:8c:b1:f6"},
{"name":"vethf80cf0f","ip-addresses":[{"ip-address-type":"ipv6","ip-address":"fe80::ac5c:95ff:fe12:924c","prefix":64}],"hardware-address":"ae:5c:95:12:92:4c"},
{"name":"veth46bb6d7","ip-addresses":[{"ip-address-type":"ipv6","ip-address":"fe80::58f2:b2ff:fe37:6ac3","prefix":64}],"hardware-address":"5a:f2:b2:37:6a:c3"}]}`

func staticCaller(reply string) Caller {
	return func(string) (string, error) { return reply, nil }
}

func TestInterfacesFiltersNoise(t *testing.T) {
	got, err := Interfaces(staticCaller(realInterfaceReply))
	if err != nil {
		t.Fatalf("Interfaces: %v", err)
	}
	if len(got) != 7 {
		t.Fatalf("want all 7 interfaces kept and labelled, got %d", len(got))
	}

	var real []string
	for _, i := range got {
		if !i.Virtual {
			real = append(real, i.Name)
		}
	}
	if len(real) != 1 || real[0] != "ens3" {
		t.Errorf("want only ens3 treated as real, got %v", real)
	}

	// ens3 keeps its routable v4 and drops the link-local v6.
	for _, i := range got {
		if i.Name != "ens3" {
			continue
		}
		if len(i.IPv4) != 1 || i.IPv4[0] != "192.168.1.13" {
			t.Errorf("ens3 IPv4 = %v, want [192.168.1.13]", i.IPv4)
		}
		if len(i.IPv6) != 0 {
			t.Errorf("ens3 IPv6 = %v, want link-local dropped", i.IPv6)
		}
	}

	// Loopback is dropped by address as well as being flagged virtual.
	for _, i := range got {
		if i.Name == "lo" && (len(i.IPv4) != 0 || len(i.IPv6) != 0) {
			t.Errorf("lo kept addresses %v %v, want none", i.IPv4, i.IPv6)
		}
	}
}

func TestFilesystemsDropsSnapsAndPseudo(t *testing.T) {
	// An Ubuntu box with snaps installed. Every squashfs sits at 100% full
	// and would swamp the real disks in the table.
	const reply = `{"return":[
{"name":"dm-0","mountpoint":"/","type":"ext4","used-bytes":8000000000,"total-bytes":40000000000},
{"name":"loop0","mountpoint":"/snap/core22/1122","type":"squashfs","used-bytes":78000000,"total-bytes":78000000},
{"name":"loop1","mountpoint":"/snap/snapd/21759","type":"squashfs","used-bytes":41000000,"total-bytes":41000000},
{"name":"tmpfs","mountpoint":"/run/user/1000","type":"tmpfs","used-bytes":100,"total-bytes":800000000},
{"name":"vda2","mountpoint":"/boot","type":"ext4","used-bytes":300000000,"total-bytes":2000000000},
{"name":"vda1","mountpoint":"/boot/efi","type":"vfat","used-bytes":6000000,"total-bytes":1000000000},
{"name":"none","mountpoint":"/proc/sys/fs/binfmt_misc","type":"binfmt_misc","used-bytes":0,"total-bytes":0}]}`

	got, err := Filesystems(staticCaller(reply))
	if err != nil {
		t.Fatalf("Filesystems: %v", err)
	}

	want := map[string]bool{"/": true, "/boot": true, "/boot/efi": true}
	if len(got) != len(want) {
		t.Fatalf("got %d filesystems %v, want %d", len(got), mounts(got), len(want))
	}
	for _, f := range got {
		if !want[f.Mountpoint] {
			t.Errorf("unexpected filesystem %q kept", f.Mountpoint)
		}
	}
}

func TestOSInfo(t *testing.T) {
	const reply = `{"return":{"kernel-release":"6.8.0-51-generic","name":"Ubuntu","pretty-name":"Ubuntu 24.04.2 LTS","version":"24.04.2 LTS (Noble Numbat)","version-id":"24.04","id":"ubuntu","machine":"x86_64"}}`

	os, kernel, err := OSInfo(staticCaller(reply))
	if err != nil {
		t.Fatalf("OSInfo: %v", err)
	}
	if os != "Ubuntu 24.04.2 LTS" {
		t.Errorf("os = %q", os)
	}
	if kernel != "6.8.0-51-generic" {
		t.Errorf("kernel = %q", kernel)
	}
}

func TestAgentErrorIsReported(t *testing.T) {
	// What an agent that doesn't know a command sends back.
	const reply = `{"error":{"class":"CommandNotFound","desc":"The command guest-get-osinfo has not been found"}}`

	if _, _, err := OSInfo(staticCaller(reply)); err == nil {
		t.Fatal("want an error for a CommandNotFound reply, got nil")
	}
}

// realWindowsFSReply is an actual guest-get-fsinfo reply from a Windows
// Server 2022 VM with two install ISOs mounted and the usual System
// Reserved volumes. It's the reason Windows filtering exists: the ISOs
// read 100% full and the letterless system partitions would swamp the one
// real disk.
const realWindowsFSReply = `{"return":[
{"name":"\\\\?\\Volume{e22d3098-a70b-11f1-abc2-806e6f6e6963}\\","total-bytes":877373440,"mountpoint":"E:\\","used-bytes":877373440,"type":"CDFS"},
{"name":"\\\\?\\Volume{e22d3097-a70b-11f1-abc2-806e6f6e6963}\\","total-bytes":6559221760,"mountpoint":"D:\\","used-bytes":6559221760,"type":"UDF"},
{"name":"\\\\?\\Volume{279e790b-c8f7-433a-88f2-3f375b5536a1}\\","total-bytes":100663296,"mountpoint":"System Reserved","used-bytes":31732736,"type":"FAT32"},
{"name":"\\\\?\\Volume{ab0b9835-9214-4fce-b7ce-7697a78d8cc4}\\","total-bytes":870313984,"mountpoint":"System Reserved","used-bytes":750354432,"type":"NTFS"},
{"name":"\\\\?\\Volume{e3f499ea-ed8f-4391-9ec7-dc0a08fced49}\\","total-bytes":52691988480,"mountpoint":"C:\\","used-bytes":14206033920,"type":"NTFS"}]}`

func TestFilesystemsDropsWindowsNoise(t *testing.T) {
	got, err := Filesystems(staticCaller(realWindowsFSReply))
	if err != nil {
		t.Fatalf("Filesystems: %v", err)
	}

	if len(got) != 1 {
		t.Fatalf("got %d filesystems %v, want 1", len(got), mounts(got))
	}
	if got[0].Mountpoint != `C:\` {
		t.Errorf("kept %q, want C:\\", got[0].Mountpoint)
	}
	if got[0].UsedBytes != 14206033920 || got[0].TotalBytes != 52691988480 {
		t.Errorf(`C:\ bytes = %d/%d, want 14206033920/52691988480`,
			got[0].UsedBytes, got[0].TotalBytes)
	}
}

// realWindowsInterfaceReply is an actual guest-network-get-interfaces reply
// from a Windows Server 2022 VM, plus a vEthernet entry of the kind WSL2 and
// Hyper-V create — Windows names its virtual adapters in a way the Linux
// prefix list never matched.
const realWindowsInterfaceReply = `{"return":[
{"name":"Ethernet","ip-addresses":[{"ip-address-type":"ipv6","ip-address":"fe80::3a11:dfb6:9b8f:8241%6","prefix":64},{"ip-address-type":"ipv4","ip-address":"192.168.1.81","prefix":24}],"hardware-address":"00:a0:98:63:a7:d1"},
{"name":"Loopback Pseudo-Interface 1","ip-addresses":[{"ip-address-type":"ipv6","ip-address":"::1","prefix":128},{"ip-address-type":"ipv4","ip-address":"127.0.0.1","prefix":8}]},
{"name":"vEthernet (WSL)","ip-addresses":[{"ip-address-type":"ipv4","ip-address":"172.29.80.1","prefix":20}],"hardware-address":"00:15:5d:ab:cd:ef"}]}`

func TestInterfacesWindowsNoise(t *testing.T) {
	got, err := Interfaces(staticCaller(realWindowsInterfaceReply))
	if err != nil {
		t.Fatalf("Interfaces: %v", err)
	}

	var real []string
	for _, i := range got {
		if !i.Virtual {
			real = append(real, i.Name)
		}
	}
	if len(real) != 1 || real[0] != "Ethernet" {
		t.Errorf("want only Ethernet treated as real, got %v", real)
	}

	// Ethernet keeps its routable v4. The v6 carries a Windows zone
	// suffix ("%6") and is dropped as link-local either way.
	for _, i := range got {
		if i.Name != "Ethernet" {
			continue
		}
		if len(i.IPv4) != 1 || i.IPv4[0] != "192.168.1.81" {
			t.Errorf("Ethernet IPv4 = %v, want [192.168.1.81]", i.IPv4)
		}
		if len(i.IPv6) != 0 {
			t.Errorf("Ethernet IPv6 = %v, want link-local dropped", i.IPv6)
		}
	}
}

func TestOSInfoWindows(t *testing.T) {
	// Actual reply from a Windows Server 2022 guest: qemu-ga fills these
	// from the registry, with the build number in kernel-release.
	const reply = `{"return":{"name":"Microsoft Windows","kernel-release":"20348","version":"Microsoft Windows Server 2022","variant":"server","pretty-name":"Windows Server 2022 Standard","version-id":"2022","variant-id":"server","kernel-version":"10.0","machine":"x86_64","id":"mswindows"}}`

	os, kernel, err := OSInfo(staticCaller(reply))
	if err != nil {
		t.Fatalf("OSInfo: %v", err)
	}
	if os != "Windows Server 2022 Standard" {
		t.Errorf("os = %q", os)
	}
	if kernel != "OS Build: 20348" {
		t.Errorf("kernel = %q, want OS Build: 20348", kernel)
	}
}

// mounts is for readable failure messages.
func mounts(fs []model.Filesystem) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, f.Mountpoint)
	}
	return out
}
