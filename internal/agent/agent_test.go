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

// mounts is for readable failure messages.
func mounts(fs []model.Filesystem) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, f.Mountpoint)
	}
	return out
}
