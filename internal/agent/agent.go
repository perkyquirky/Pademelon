// Package agent speaks the QEMU guest agent's JSON protocol.
//
// It deliberately knows nothing about libvirt. Callers hand in a Caller that
// gets a command string to the agent and brings the reply back, which keeps
// this package easy to test with canned JSON.
//
// Deliberately absent: guest-exec. It is remote code execution into the
// guest, and nothing here needs it. Don't add it.
package agent

import (
	"encoding/json"
	"fmt"
	"net"
	"strings"

	"pademelon/internal/model"
)

// Caller sends one JSON command to a guest agent and returns the raw reply.
type Caller func(cmd string) (string, error)

// call runs cmd and unwraps the {"return": ...} envelope into out.
func call(c Caller, cmd string, out any) error {
	raw, err := c(cmd)
	if err != nil {
		return err
	}
	var env struct {
		Return json.RawMessage `json:"return"`
		Error  *struct {
			Class string `json:"class"`
			Desc  string `json:"desc"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		return fmt.Errorf("decode reply: %w", err)
	}
	if env.Error != nil {
		return fmt.Errorf("agent error: %s: %s", env.Error.Class, env.Error.Desc)
	}
	if len(env.Return) == 0 {
		return fmt.Errorf("empty reply")
	}
	if err := json.Unmarshal(env.Return, out); err != nil {
		return fmt.Errorf("decode return: %w", err)
	}
	return nil
}

// Ping checks the agent is actually answering. Cheap, and a good first call
// so we don't attribute a dead agent to whichever command happened to be
// first in the list.
func Ping(c Caller) error {
	// guest-ping replies with an empty object, which call() would reject as
	// an empty return, so check the raw reply here instead.
	raw, err := c(`{"execute":"guest-ping"}`)
	if err != nil {
		return err
	}
	var env struct {
		Return *json.RawMessage `json:"return"`
		Error  *struct {
			Desc string `json:"desc"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		return fmt.Errorf("decode ping reply: %w", err)
	}
	if env.Error != nil {
		return fmt.Errorf("agent error: %s", env.Error.Desc)
	}
	if env.Return == nil {
		return fmt.Errorf("unexpected ping reply: %.80s", raw)
	}
	return nil
}

// Hostname returns the guest's hostname.
func Hostname(c Caller) (string, error) {
	var r struct {
		HostName string `json:"host-name"`
	}
	if err := call(c, `{"execute":"guest-get-host-name"}`, &r); err != nil {
		return "", err
	}
	return r.HostName, nil
}

// OSInfo returns the guest's pretty OS name and kernel release, e.g.
// "Ubuntu 24.04.2 LTS" and "6.8.0-51-generic".
func OSInfo(c Caller) (osName, kernel string, err error) {
	var r struct {
		PrettyName    string `json:"pretty-name"`
		Name          string `json:"name"`
		Version       string `json:"version"`
		KernelRelease string `json:"kernel-release"`
	}
	if err := call(c, `{"execute":"guest-get-osinfo"}`, &r); err != nil {
		return "", "", err
	}
	osName = r.PrettyName
	if osName == "" {
		osName = strings.TrimSpace(r.Name + " " + r.Version)
	}
	return osName, r.KernelRelease, nil
}

// Interfaces returns the guest's network interfaces, with the container and
// virtual ones flagged rather than dropped — the web layer decides what to
// show, we just label them.
func Interfaces(c Caller) ([]model.Iface, error) {
	var r []struct {
		Name string `json:"name"`
		MAC  string `json:"hardware-address"`
		IPs  []struct {
			Type    string `json:"ip-address-type"`
			Address string `json:"ip-address"`
			Prefix  int    `json:"prefix"`
		} `json:"ip-addresses"`
	}
	if err := call(c, `{"execute":"guest-network-get-interfaces"}`, &r); err != nil {
		return nil, err
	}

	out := make([]model.Iface, 0, len(r))
	for _, in := range r {
		iface := model.Iface{
			Name:    in.Name,
			MAC:     in.MAC,
			Virtual: isVirtualIface(in.Name, in.MAC),
		}
		for _, a := range in.IPs {
			if skipAddr(a.Address) {
				continue
			}
			switch a.Type {
			case "ipv4":
				iface.IPv4 = append(iface.IPv4, a.Address)
			case "ipv6":
				iface.IPv6 = append(iface.IPv6, a.Address)
			}
		}
		out = append(out, iface)
	}
	return out, nil
}

// Filesystems returns the guest's real mounted filesystems.
//
// Ubuntu is the reason for the filtering here: a stock 24.04 box reports
// every snap as a squashfs loop mount sitting at exactly 100% full, which
// would drown the real disks in a dashboard.
func Filesystems(c Caller) ([]model.Filesystem, error) {
	var r []struct {
		Name       string `json:"name"`
		Mountpoint string `json:"mountpoint"`
		Type       string `json:"type"`
		UsedBytes  uint64 `json:"used-bytes"`
		TotalBytes uint64 `json:"total-bytes"`
	}
	if err := call(c, `{"execute":"guest-get-fsinfo"}`, &r); err != nil {
		return nil, err
	}

	out := make([]model.Filesystem, 0, len(r))
	for _, in := range r {
		if skipFilesystem(in.Type, in.Mountpoint, in.TotalBytes) {
			continue
		}
		out = append(out, model.Filesystem{
			Mountpoint: in.Mountpoint,
			Type:       in.Type,
			UsedBytes:  in.UsedBytes,
			TotalBytes: in.TotalBytes,
		})
	}
	return out, nil
}

// virtualIfacePrefixes are interfaces created by container and VM runtimes.
// They're real, they're just never the answer to "what IP is this box on".
var virtualIfacePrefixes = []string{
	"docker", "br-", "veth", "virbr", "cni", "flannel", "cali", "tap", "kube",
}

func isVirtualIface(name, mac string) bool {
	if name == "lo" || strings.HasPrefix(name, "lo:") {
		return true
	}
	// An all-zero MAC means loopback or something equally uninteresting.
	if mac == "" || mac == "00:00:00:00:00:00" {
		return true
	}
	for _, p := range virtualIfacePrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// skipAddr drops addresses that tell you nothing: loopback, and IPv6
// link-local, which every interface has and nobody ever connects to.
func skipAddr(addr string) bool {
	ip := net.ParseIP(addr)
	if ip == nil {
		return true
	}
	return ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified()
}

// pseudoFilesystems never represent real storage.
var pseudoFilesystems = map[string]bool{
	"squashfs": true, "tmpfs": true, "devtmpfs": true, "overlay": true,
	"ramfs": true, "autofs": true, "efivarfs": true, "configfs": true,
	"debugfs": true, "tracefs": true, "securityfs": true, "pstore": true,
	"bpf": true, "cgroup": true, "cgroup2": true, "mqueue": true,
	"hugetlbfs": true, "proc": true, "sysfs": true, "devpts": true,
	"binfmt_misc": true, "fusectl": true, "nsfs": true, "fuse.snapfuse": true,
	"iso9660": true,
}

// pseudoMounts are trees that are never worth a row in the table.
var pseudoMounts = []string{"/snap/", "/sys/", "/proc/", "/dev/", "/run/", "/var/lib/docker/"}

func skipFilesystem(fsType, mount string, total uint64) bool {
	if total == 0 {
		return true
	}
	if pseudoFilesystems[strings.ToLower(fsType)] {
		return true
	}
	for _, p := range pseudoMounts {
		if strings.HasPrefix(mount, p) {
			return true
		}
	}
	return false
}
